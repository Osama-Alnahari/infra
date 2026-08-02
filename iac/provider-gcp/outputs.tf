output "external_dns_records" {
  description = "DNS records to create in DigitalOcean for the externally managed Etlaq E2B subdomain."
  value       = module.cluster.external_dns_records
}
