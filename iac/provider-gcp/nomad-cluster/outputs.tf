output "shared_chunk_cache_path" {
  value = var.filestore_cache_enabled ? "${local.nfs_mount_path}/${local.nfs_mount_subdir}" : ""
}

output "external_dns_records" {
  value = module.network.external_dns_records
}
