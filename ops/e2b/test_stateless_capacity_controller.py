import tempfile
import unittest
from pathlib import Path

from stateless_capacity_controller import Config, decide


def config(**changes):
    base = Config(
        project="p", region="r", mig="m", worker_prefix="stateless-",
        api_url="https://example", api_admin_token="secret",
        nomad_url="http://nomad", nomad_token="secret",
        state_path=Path(tempfile.gettempdir()) / "state.json",
    )
    return base.__class__(**{**base.__dict__, **changes})


def node(name, running=0, starting=0, status="ready"):
    return {"id": name, "sandboxCount": running, "sandboxStartingCount": starting, "status": status}


class DecisionTests(unittest.TestCase):
    def test_restores_minimum_worker_before_cooldown_or_registration_checks(self):
        c = config(cooldown_seconds=180)
        result = decide(
            c,
            0,
            [node("legacy", 0)],
            {"last_mutation": 99, "draining": None},
            100,
            set(),
        )
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["size"], 1)
        self.assertEqual(result["reason"], "below_minimum")

    def test_restores_minimum_worker_before_clearing_stale_drain_state(self):
        c = config(cooldown_seconds=180)
        result = decide(
            c,
            0,
            [node("legacy", 0), node("stateless-old", 0)],
            {"last_mutation": 99, "draining": "stateless-old"},
            100,
            set(),
        )
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["size"], 1)
        self.assertEqual(result["reason"], "below_minimum")

    def test_scales_out_at_slot_pressure(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 1, [node("legacy", 24), node("stateless-a", 9)], {"last_mutation": 0, "draining": None}, 100)
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["size"], 2)

    def test_scales_out_when_one_stateless_worker_reaches_fourteen(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 1, [node("legacy", 0), node("stateless-a", 14)], {"last_mutation": 0, "draining": None}, 100)
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["reason"], "worker_saturation")
        self.assertEqual(result["worker"], "stateless-a")

    def test_saturated_worker_is_not_counted_twice(self):
        c = config(cooldown_seconds=0)
        result = decide(
            c,
            2,
            [node("legacy", 5), node("stateless-a", 14), node("stateless-b", 0)],
            {"last_mutation": 0, "draining": None, "saturation_scaled": ["stateless-a"]},
            100,
        )
        self.assertEqual(result["action"], "none")

    def test_scales_out_when_fleet_has_only_eight_free_slots(self):
        c = config(cooldown_seconds=0)
        result = decide(
            c,
            1,
            [node("legacy", 14), node("stateless-a", 10)],
            {"last_mutation": 0, "draining": None},
            100,
        )
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["reason"], "fleet_headroom")

    def test_does_not_scale_out_with_nine_free_slots(self):
        c = config(cooldown_seconds=0)
        result = decide(
            c,
            1,
            [node("legacy", 13), node("stateless-a", 10)],
            {"last_mutation": 0, "draining": None},
            100,
        )
        self.assertEqual(result["action"], "none")
        self.assertEqual(result["reason"], "within_band")

    def test_never_exceeds_five(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 5, [node("legacy", 24), node("stateless-a", 80)], {"last_mutation": 0, "draining": None}, 100)
        self.assertEqual(result["action"], "none")

    def test_scale_in_first_marks_lowest_worker_draining(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 2, [node("legacy", 4), node("stateless-a", 2), node("stateless-b", 0)], {"last_mutation": 0, "draining": None}, 100)
        self.assertEqual(result, {"action": "start_draining", "worker": "stateless-b", "reason": "low_slot_pressure", "active": 6})

    def test_draining_worker_is_not_deleted_until_empty(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 2, [node("stateless-a", 1)], {"last_mutation": 0, "draining": "stateless-a"}, 100)
        self.assertEqual(result["action"], "wait_draining")

    def test_exact_empty_draining_worker_is_deleted(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 2, [node("stateless-a", 0)], {"last_mutation": 0, "draining": "stateless-a"}, 100)
        self.assertEqual(result["action"], "delete")

    def test_minimum_worker_is_never_deleted(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 1, [node("stateless-a", 0)], {"last_mutation": 0, "draining": "stateless-a"}, 100)
        self.assertEqual(result["action"], "wait_draining")

    def test_zero_minimum_deletes_last_worker_after_it_drains(self):
        c = config(cooldown_seconds=0, min_workers=0)
        result = decide(c, 1, [node("stateless-a", 0)], {"last_mutation": 0, "draining": "stateless-a"}, 100)
        self.assertEqual(result["action"], "delete")

    def test_zero_workers_remain_off_while_legacy_capacity_has_headroom(self):
        c = config(cooldown_seconds=0, min_workers=0)
        result = decide(c, 0, [node("legacy", 2)], {"last_mutation": 0, "draining": None}, 100, set())
        self.assertEqual(result["action"], "none")
        self.assertEqual(result["reason"], "within_band")

    def test_zero_workers_scale_out_when_legacy_headroom_reaches_floor(self):
        c = config(cooldown_seconds=0, min_workers=0)
        result = decide(c, 0, [node("legacy", 7)], {"last_mutation": 0, "draining": None}, 100, set())
        self.assertEqual(result["action"], "scale_out")
        self.assertEqual(result["size"], 1)
        self.assertEqual(result["reason"], "fleet_headroom")

    def test_waits_for_every_mig_worker_to_register_before_scaling(self):
        c = config(cooldown_seconds=0)
        result = decide(c, 2, [node("stateless-a", 0)], {"last_mutation": 0, "draining": None}, 100)
        self.assertEqual(result["action"], "none")
        self.assertEqual(result["reason"], "worker_registration_pending")

    def test_stale_api_node_is_cleared_after_exact_mig_deletion(self):
        c = config(cooldown_seconds=0)
        result = decide(
            c,
            1,
            [node("stateless-old", 2), node("stateless-current", 0)],
            {"last_mutation": 0, "draining": "stateless-old"},
            100,
            {"stateless-current"},
        )
        self.assertEqual(result["action"], "clear_draining")
        self.assertEqual(result["reason"], "worker_absent_from_mig")


if __name__ == "__main__":
    unittest.main()
