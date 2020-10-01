package openstack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

const (
	authHearName = "X-Auth-Token"
)

type apiCredentials struct {
	computeURL *url.URL
	token      string
	expiration time.Time
}

type apiConfig struct {
	client       *http.Client
	port         int
	tokenLock    sync.Mutex
	creds        *apiCredentials
	authTokenReq []byte
	endpoint     *url.URL
	allTenants   bool
	region       string
	availability string
}

func (cfg *apiConfig) getFreshAPICredentials() (*apiCredentials, error) {
	cfg.tokenLock.Lock()
	defer cfg.tokenLock.Unlock()

	if time.Until(cfg.creds.expiration) > 10*time.Second {
		// Credentials aren't expired yet.
		return cfg.creds, nil
	}
	newCreds, err := getToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed token refresh: %w", err)
	}
	logger.Infof("refreshed, next : %v", cfg.creds.expiration.String())

	cfg.creds = newCreds

	return cfg.creds, nil
}

/*
{ "auth": {
    "identity": {
      "methods": ["password"],
      "password": {
        "user": {
          "name": "admin",
          "domain": { "id": "default" },
          "password": "adminpwd"
        }
      }
    }
  }
}
{ "auth": {
    "identity": {
      "methods": ["password"],
      "password": {
        "user": {
          "name": "admin",
          "domain": { "id": "default" },
          "password": "adminpwd"
        }
      }
    },
    "scope": {
      "project": {
        "name": "demo",
        "domain": { "id": "default" }
      }
    }
  }
}
{ "auth": {
    "identity": {
      "methods": ["token"],
      "token": {
        "id": "'$OS_TOKEN'"
      }
    }
  }
}
*/

type Endpoint struct {
	ID         string
	RegionID   string
	RegionName string
	URL        string
	Name       string
	Type       string
	Interface  string
}

type AuthResp struct {
	Token struct {
		ExpiresAt time.Time `json:"expires_at,omitempty"`
		Catalog   []struct {
			Type      string
			Name      string
			Endpoints []Endpoint `json:"endpoints"`
		} `json:"catalog,omitempty"`
	}
}

func (ar AuthResp) novaEndpoint(availability string, region string) *Endpoint {
	for _, eps := range ar.Token.Catalog {
		if eps.Name == "nova" {
			for _, ep := range eps.Endpoints {
				if ep.Interface == availability && (region == "" || region == ep.RegionID || region == ep.RegionName) {
					return &ep
				}
			}
		}
	}
	return nil
}

var configMap = discoveryutils.NewConfigMap()

func getAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	v, err := configMap.Get(sdc, func() (interface{}, error) { return newAPIConfig(sdc, baseDir) })
	if err != nil {
		return nil, err
	}
	return v.(*apiConfig), nil
}

func newAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	cfg := &apiConfig{
		client: discoveryutils.GetHTTPClient(),
	}
	if sdc.TLSConfig != nil {
		config, err := promauth.NewConfig(baseDir, nil, "", "", sdc.TLSConfig)
		if err != nil {
			return nil, err
		}
		tr := &http.Transport{
			TLSClientConfig: config.NewTLSConfig(),
		}
		cfg.client.Transport = tr
	}
	if len(cfg.availability) == 0 {
		cfg.availability = "public"
	}
	parsedURL, err := url.Parse(sdc.IdentityEndpoint)
	if err != nil {
		return nil, err
	}
	cfg.endpoint = parsedURL
	//	tokenReq, err := buildAuthRequest(sdc)
	tokenReq, err := buildAuthRequestBody(sdc)
	if err != nil {
		return nil, err
	}
	cfg.authTokenReq = tokenReq
	token, err := getToken(cfg)
	if err != nil {
		return nil, err
	}
	cfg.creds = token
	return cfg, nil
}

func getToken(cfg *apiConfig) (*apiCredentials, error) {

	apiURL := *cfg.endpoint
	apiURL.Path = path.Join(apiURL.Path, "auth", "tokens")

	resp, err := cfg.client.Post(apiURL.String(), "application/json", bytes.NewBuffer(cfg.authTokenReq))
	if err != nil {
		return nil, err
	}
	r, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	at := resp.Header.Get("X-Subject-Token")

	var aur AuthResp
	if err := json.Unmarshal(r, &aur); err != nil {
		return nil, fmt.Errorf("cannot parsed auth credentials response: %w", err)
	}

	novaEndpoint := aur.novaEndpoint(cfg.availability, cfg.region)
	if novaEndpoint == nil {
		logger.Infof("resp: %v", aur.Token)
		return nil, errors.New("Cannot get novaEndpoint, not enough permissions?")
	}

	parsedURL, err := url.Parse(novaEndpoint.URL)
	return &apiCredentials{
		token:      at,
		expiration: aur.Token.ExpiresAt,
		computeURL: parsedURL,
	}, nil
}

func readCredentialsFromEnv() *SDConfig {
	authURL := os.Getenv("OS_AUTH_URL")
	username := os.Getenv("OS_USERNAME")
	userID := os.Getenv("OS_USERID")
	password := os.Getenv("OS_PASSWORD")
	//passcode := os.Getenv("OS_PASSCODE")
	// TODO
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
		IdentityEndpoint: authURL,
		Username:         username,
		UserID:           userID,
		Password:         password,

		ProjectName:                 tenantName,
		ProjectID:                   tenantID,
		DomainName:                  domainName,
		DomainID:                    domainID,
		ApplicationCredentialName:   applicationCredentialName,
		ApplicationCredentialID:     applicationCredentialID,
		ApplicationCredentialSecret: applicationCredentialSecret,
	}
}

func readResponseBody(resp *http.Response, apiURL string) ([]byte, error) {
	data, err := ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("cannot read response from %q: %w", apiURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code for %q; got %d; want %d; response body: %q",
			apiURL, resp.StatusCode, http.StatusOK, data)
	}
	return data, nil
}

func buildAuthRequestBody(opts *SDConfig) ([]byte, error) {
	type domainReq struct {
		ID   *string `json:"id,omitempty"`
		Name *string `json:"name,omitempty"`
	}

	type userReq struct {
		ID       *string    `json:"id,omitempty"`
		Name     *string    `json:"name,omitempty"`
		Password *string    `json:"password,omitempty"`
		Passcode *string    `json:"passcode,omitempty"`
		Domain   *domainReq `json:"domain,omitempty"`
	}

	type passwordReq struct {
		User userReq `json:"user"`
	}

	type tokenReq struct {
		ID string `json:"id"`
	}

	type applicationCredentialReq struct {
		ID     *string  `json:"id,omitempty"`
		Name   *string  `json:"name,omitempty"`
		User   *userReq `json:"user,omitempty"`
		Secret *string  `json:"secret,omitempty"`
	}

	type identityReq struct {
		Methods               []string                  `json:"methods"`
		Password              *passwordReq              `json:"password,omitempty"`
		Token                 *tokenReq                 `json:"token,omitempty"`
		ApplicationCredential *applicationCredentialReq `json:"application_credential,omitempty"`
	}

	type authReq struct {
		Identity identityReq            `json:"identity"`
		Scope    map[string]interface{} `json:"scope"`
	}

	type request struct {
		Auth authReq `json:"auth"`
	}

	// Populate the request structure based on the provided arguments. Create and return an error
	// if insufficient or incompatible information is present.
	var req request

	if opts.Password == "" {
		if opts.ApplicationCredentialID != "" {
			// Configure the request for ApplicationCredentialID authentication.
			// https://github.com/openstack/keystoneauth/blob/stable/rocky/keystoneauth1/identity/v3/application_credential.py#L48-L67
			// There are three kinds of possible application_credential requests
			// 1. application_credential id + secret
			// 2. application_credential name + secret + user_id
			// 3. application_credential name + secret + username + domain_id / domain_name
			if opts.ApplicationCredentialSecret == "" {
				return nil, fmt.Errorf("ApplicationCredentialSecret is empty")
			}
			req.Auth.Identity.Methods = []string{"application_credential"}
			req.Auth.Identity.ApplicationCredential = &applicationCredentialReq{
				ID:     &opts.ApplicationCredentialID,
				Secret: &opts.ApplicationCredentialSecret,
			}
		} else if opts.ApplicationCredentialName != "" {
			if opts.ApplicationCredentialSecret == "" {
				return nil, fmt.Errorf("ApplicationCredentialName is not empty and ApplicationCredentialSecret is empty")
			}

			var userRequest *userReq

			if opts.UserID != "" {
				// UserID could be used without the domain information
				userRequest = &userReq{
					ID: &opts.UserID,
				}
			}

			if userRequest == nil && opts.Username == "" {
				// Make sure that Username or UserID are provided
				return nil, fmt.Errorf("username is empty")
			}

			if userRequest == nil && opts.DomainID != "" {
				userRequest = &userReq{
					Name:   &opts.Username,
					Domain: &domainReq{ID: &opts.DomainID},
				}
			}

			if userRequest == nil && opts.DomainName != "" {
				userRequest = &userReq{
					Name:   &opts.Username,
					Domain: &domainReq{Name: &opts.DomainName},
				}
			}

			// Make sure that DomainID or DomainName are provided among Username
			if userRequest == nil {
				return nil, fmt.Errorf("domainID and DomainName is empty")
			}

			req.Auth.Identity.Methods = []string{"application_credential"}
			req.Auth.Identity.ApplicationCredential = &applicationCredentialReq{
				Name:   &opts.ApplicationCredentialName,
				User:   userRequest,
				Secret: &opts.ApplicationCredentialSecret,
			}
		} else {
			// If no password or token ID or ApplicationCredential are available, authentication can't continue.
			return nil, fmt.Errorf("password is missing")
		}
	} else {
		// Password authentication.
		if opts.Password != "" {
			req.Auth.Identity.Methods = append(req.Auth.Identity.Methods, "password")
		}

		// At least one of Username and UserID must be specified.
		if opts.Username == "" && opts.UserID == "" {
			return nil, fmt.Errorf("username and userid is empty")
		}

		if opts.Username != "" {
			// If Username is provided, UserID may not be provided.
			if opts.UserID != "" {
				return nil, fmt.Errorf("both username and userID is present")
			}

			// Either DomainID or DomainName must also be specified.
			if opts.DomainID == "" && opts.DomainName == "" {
				return nil, fmt.Errorf(" domain_id and domain_name is missing")
			}

			if opts.DomainID != "" {
				if opts.DomainName != "" {
					return nil, fmt.Errorf("both domain_id and domain_name is present")
				}

				// Configure the request for Username and Password authentication with a DomainID.
				if opts.Password != "" {
					req.Auth.Identity.Password = &passwordReq{
						User: userReq{
							Name:     &opts.Username,
							Password: &opts.Password,
							Domain:   &domainReq{ID: &opts.DomainID},
						},
					}
				}
			}

			if opts.DomainName != "" {
				// Configure the request for Username and Password authentication with a DomainName.
				if opts.Password != "" {
					req.Auth.Identity.Password = &passwordReq{
						User: userReq{
							Name:     &opts.Username,
							Password: &opts.Password,
							Domain:   &domainReq{Name: &opts.DomainName},
						},
					}
				}
			}
		}

		if opts.UserID != "" {
			// If UserID is specified, neither DomainID nor DomainName may be.
			if opts.DomainID != "" {
				return nil, fmt.Errorf("both user_id and domain_id is present")
			}
			if opts.DomainName != "" {
				return nil, fmt.Errorf("both user_id and domain_name is present")
			}

			// Configure the request for UserID and Password authentication.
			if opts.Password != "" {
				req.Auth.Identity.Password = &passwordReq{
					User: userReq{
						ID:       &opts.UserID,
						Password: &opts.Password,
					},
				}
			}

		}
	}
	scope, err := buildScope(opts)
	if err != nil {
		return nil, err
	}
	if len(scope) > 0 {
		req.Auth.Scope = scope
	}

	return json.Marshal(req)
}

func buildScope(cfg *SDConfig) (map[string]interface{}, error) {

	if cfg.ProjectName != "" {
		// ProjectName provided: either DomainID or DomainName must also be supplied.
		// ProjectID may not be supplied.
		if cfg.DomainID == "" && cfg.DomainName == "" {
			return nil, fmt.Errorf("both domain_id and domain_name present")
		}
		if cfg.ProjectID != "" {
			return nil, fmt.Errorf("both domain_id and domain_name present")
		}

		if cfg.DomainID != "" {
			// ProjectName + DomainID
			return map[string]interface{}{
				"project": map[string]interface{}{
					"name":   &cfg.ProjectName,
					"domain": map[string]interface{}{"id": &cfg.DomainID},
				},
			}, nil
		}

		if cfg.DomainName != "" {
			// ProjectName + DomainName
			return map[string]interface{}{
				"project": map[string]interface{}{
					"name":   &cfg.ProjectName,
					"domain": map[string]interface{}{"name": &cfg.DomainName},
				},
			}, nil
		}
	} else if cfg.ProjectID != "" {
		// ProjectID provided. ProjectName, DomainID, and DomainName may not be provided.
		if cfg.DomainID != "" {
			return nil, fmt.Errorf("both domain_id and domain_name present")
		}
		if cfg.DomainName != "" {
			return nil, fmt.Errorf("both domain_id and domain_name present")
		}

		// ProjectID
		return map[string]interface{}{
			"project": map[string]interface{}{
				"id": &cfg.ProjectID,
			},
		}, nil
	} else if cfg.DomainID != "" {
		// DomainID provided. ProjectID, ProjectName, and DomainName may not be provided.
		if cfg.DomainName != "" {
			return nil, fmt.Errorf("both domain_id and domain_name present")
		}

		// DomainID
		return map[string]interface{}{
			"domain": map[string]interface{}{
				"id": &cfg.DomainID,
			},
		}, nil
	} else if cfg.DomainName != "" {
		// DomainName
		return map[string]interface{}{
			"domain": map[string]interface{}{
				"name": &cfg.DomainName,
			},
		}, nil
	}
	return nil, nil

}
