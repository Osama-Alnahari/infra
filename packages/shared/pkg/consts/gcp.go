package consts

import "os"

var (
	GCPProject                 = os.Getenv("GCP_PROJECT_ID")
	Domain                     = os.Getenv("DOMAIN_NAME")
	DockerRegistry             = os.Getenv("GCP_DOCKER_REPOSITORY_NAME")
	GoogleServiceAccountSecret = os.Getenv("GOOGLE_SERVICE_ACCOUNT_BASE64")
	GCPServiceAccountEmail     = os.Getenv("GCP_SERVICE_ACCOUNT_EMAIL")
	DockerAuthConfig           = os.Getenv("DOCKER_AUTH_BASE64")
	GCPRegion                  = os.Getenv("GCP_REGION")
)
