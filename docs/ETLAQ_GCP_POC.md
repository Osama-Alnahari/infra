# Etlaq E2B GCP proof of concept

This deployment is an isolated, non-HA E2B environment for lifecycle and
pause/resume testing in GCP Dammam (`me-central2`). It must not reuse the
Studio2 GKE preview domain or the existing Firecracker test domain.

## Ownership and boundaries

- E2B owns sandbox creation, routing, template builds, snapshots and resume.
- Studio2 remains an external API client and keeps its existing database.
- The E2B control plane uses its own PostgreSQL database and resource prefix.
- Sandbox/template artifacts and durable pause snapshots live in E2B's GCS
  buckets. The client worker's Local SSD is only the hot snapshot/template
  cache and can be lost safely.
- The standalone Etlaq agent remains outside E2B sandboxes.

## Reduced topology

| Pool | Count | Machine | Notes |
| --- | ---: | --- | --- |
| Nomad/Consul server | 1 | `e2-standard-2` | No HA; control-plane downtime during replacement |
| API | 1 | `e2-standard-4` | Hosts API, proxy, ingress, Redis, Loki and telemetry |
| Build | 1 | `n2-standard-4` | 100 GiB PD-SSD cache; may scale to zero after template builds |
| Client | 1 | `n2-standard-4` | One 375 GiB Local SSD for meaningful hot-resume tests |
| ClickHouse | 0 | disabled | Product analytics unavailable |
| Dedicated Loki pool | 0 | disabled | Loki job remains colocated on the API pool |
| Dashboard API | 0 | disabled | Not needed by Studio2 |
| Filestore | 0 | disabled | Not used for sandbox pause/resume |

The API node remains `e2-standard-4`: the upstream jobs colocate API,
client-proxy, ingress, Redis, Loki and OTel on this node, and their default
resource reservations do not fit safely on two vCPUs.

## Naming

- Prefix: `etlaq-e2b-poc-`
- Domain: `e2b.etlaq.sa`
- Runtime network: `etlaq-e2b-poc-net`
- Terraform state bucket: `gen-lang-client-0975136333-etlaq-e2b-poc-tfstate`
- PostgreSQL instance: `etlaq-e2b-poc-postgres`

The temporary Packer VM also uses `n2-standard-4`; upstream's default
`n1-standard-4` is unavailable in Dammam.

The domain must be delegated or managed in a Cloudflare zone because upstream
Terraform uses the Cloudflare provider for DNS authorization and wildcard
records. Existing `*.preview.etlaq.sa` and `*.sandbox.etlaq.sa` records must not
be changed.

## Resume tests

1. Hot resume: pause and resume while the origin client remains healthy. The
   origin node's Local SSD should satisfy the snapshot reads.
2. Cold resume: pause, wait for the asynchronous GCS upload to finish, replace
   the only client worker, then resume and verify persisted workspace data.
3. Record create, pause, API resume and first successful public preview times
   separately.

## Rollback

Run Terraform destroy using the same `.env.dev` and state bucket. Delete only
resources with the `etlaq-e2b-poc-` prefix, then remove the dedicated DNS zone,
PostgreSQL instance and runtime network. Never delete shared Studio2, GKE,
GitLab or `etlaq-fc-test-*` resources.
