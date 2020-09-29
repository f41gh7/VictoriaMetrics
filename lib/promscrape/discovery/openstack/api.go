package openstack

import (
	"net/http"
	"os"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

type apiConfig struct {
	client       *http.Client
	zones        []string
	project      string
	filter       string
	tagSeparator string
	port         int
}

var configMap = discoveryutils.NewConfigMap()

func getAPIConfig(sdc *SDConfig) (*apiConfig, error) {
	v, err := configMap.Get(sdc, func() (interface{}, error) { return newAPIConfig(sdc) })
	if err != nil {
		return nil, err
	}
	return v.(*apiConfig), nil
}

func newAPIConfig(sdc *SDConfig) (*apiConfig, error) {
	return nil, nil
}

func readCredentialsFromEnv()*SDConfig{
	authURL := os.Getenv("OS_AUTH_URL")
	username := os.Getenv("OS_USERNAME")
	userID := os.Getenv("OS_USERID")
	password := os.Getenv("OS_PASSWORD")
	passcode := os.Getenv("OS_PASSCODE")
	tenantID := os.Getenv("OS_TENANT_ID")
	tenantName := os.Getenv("OS_TENANT_NAME")
	domainID := os.Getenv("OS_DOMAIN_ID")
	domainName := os.Getenv("OS_DOMAIN_NAME")
	applicationCredentialID := os.Getenv("OS_APPLICATION_CREDENTIAL_ID")
	applicationCredentialName := os.Getenv("OS_APPLICATION_CREDENTIAL_NAME")
	applicationCredentialSecret := os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")
	// If OS_PROJECT_ID is set, overwrite tenantID with the value.
	if v := os.Getenv("OS_PROJECT_ID"); v != "" {
		tenantID = v
	}

	// If OS_PROJECT_NAME is set, overwrite tenantName with the value.
	if v := os.Getenv("OS_PROJECT_NAME"); v != "" {
		tenantName = v
	}
	return &SDConfig{
		IdentityEndpoint:            authURL,
		Username:                    username,
		UserID:                      userID,
		Password:                    password,
		ProjectName:                 tenantName,
		ProjectID:                   tenantID,
		DomainName:                  domainName,
		DomainID:                    domainID,
		ApplicationCredentialName:   applicationCredentialName,
		ApplicationCredentialID:     applicationCredentialID,
		ApplicationCredentialSecret: applicationCredentialSecret,
		Role:                        ,
		Region:                      "",
		RefreshInterval:             0,
		Port:                        0,
		AllTenants:                  false,
		TLSConfig:                   nil,
		Availability:                "",
	}
}