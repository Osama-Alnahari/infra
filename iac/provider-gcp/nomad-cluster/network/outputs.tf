output "external_dns_records" {
  description = "DNS records to create in the external authoritative DNS provider when Cloudflare management is disabled."
  value = var.manage_cloudflare_dns ? {} : merge(
    {
      certificate_authorization = {
        name  = google_certificate_manager_dns_authorization.dns_auth.dns_resource_record[0].name
        type  = google_certificate_manager_dns_authorization.dns_auth.dns_resource_record[0].type
        value = google_certificate_manager_dns_authorization.dns_auth.dns_resource_record[0].data
      }
      wildcard = {
        name  = local.is_subdomain ? "*.${local.subdomain}.${local.root_domain}" : "*.${local.root_domain}"
        type  = "A"
        value = google_compute_global_forwarding_rule.https.ip_address
      }
    },
    {
      for key, route in local.routing_matrix : "ingress_${replace(key, "|", "_")}" => {
        name  = "${route.record_name}.${route.root_domain}"
        type  = "A"
        value = google_compute_global_forwarding_rule.ingress.ip_address
      }
    }
  )
}
