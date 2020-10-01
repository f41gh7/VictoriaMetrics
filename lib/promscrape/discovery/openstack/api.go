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
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

const (
	authHearName = "X-Auth-Token"
)

type apiConfig struct {
	client          *http.Client
	port            int
	tokenLock       sync.Mutex
	authToken       string
	authTokenReq    []byte
	endpoint        string
	novaEndpoint    string
	project         string
	allTenants      bool
	region          string
	tagSeparator    string
	availability    string
	domain          string
	tokenExpiration time.Time
}

func (cfg *apiConfig) getFreshAPICredentials() (string, error) {
	cfg.tokenLock.Lock()
	defer cfg.tokenLock.Unlock()

	if time.Until(cfg.tokenExpiration) > 10*time.Second {
		// Credentials aren't expired yet.
		return cfg.authToken, nil
	}
	newToken, err := getToken(cfg)
	if err != nil {
		return "", fmt.Errorf("failed token refresh: %w", err)
	}

	cfg.authToken = newToken

	return cfg.authToken, nil
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

type serviceResp struct {
	Services []struct {
		Name  string
		Type  string
		Links struct {
			Self url.URL
		}
	}
}
type Endpoint struct {
	ID         string
	RegionID   string
	RegionName string
	URL        string
	Name       string
	Type       string
	Interface  string
}
type Domain struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type Token struct {
	ID   string
	Name string
}
type authRequest struct {
	Auth struct {
		Identity struct {
			Methods  []string `json:"methods,omitempty"`
			Token    *Token   `json:"token,omitempty"`
			Password struct {
				User struct {
					Name     string  `json:"name,omitempty"`
					Password string  `json:"password,omitempty"`
					Domain   *Domain `json:"domain,omitempty"`
				} `json:"user,omitempty"`
			} `json:"password,omitempty"`
		} `json:"identity,omitempty"`
		Scope struct {
			Domain  *Domain `json:"domain,omitempty"`
			Project struct {
				Name   string  `json:"name,omitempty"`
				Domain *Domain `json:"domain,omitempty"`
			} `json:"project,omitempty"`
		} `json:"scope,omitempty"`
	} `json:"auth"`
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

func getAPIConfig(sdc *SDConfig) (*apiConfig, error) {
	v, err := configMap.Get(sdc, func() (interface{}, error) { return newAPIConfig(sdc) })
	if err != nil {
		return nil, err
	}
	return v.(*apiConfig), nil
}

func newAPIConfig(sdc *SDConfig) (*apiConfig, error) {
	ac := &apiConfig{
		client:   discoveryutils.GetHTTPClient(),
		endpoint: sdc.IdentityEndpoint,
		domain:   sdc.DomainName,
		project:  sdc.ProjectName,
	}
	if len(ac.availability) == 0 {
		ac.availability = "public"
	}
	tokenReq, err := buildAuthRequest(sdc)
	if err != nil {
		return nil, err
	}
	ac.authTokenReq = tokenReq
	token, err := getToken(ac)
	if err != nil {
		return nil, err
	}
	ac.authToken = token
	return ac, nil
}

func buildAuthRequest(sdc *SDConfig) ([]byte, error) {
	req := &authRequest{}

	d := &Domain{Name: sdc.DomainName}
	req.Auth.Identity.Methods = []string{"password"}
	req.Auth.Identity.Password.User.Password = sdc.Password
	req.Auth.Identity.Password.User.Name = sdc.Username
	req.Auth.Identity.Password.User.Domain = d
	req.Auth.Scope.Project.Name = sdc.ProjectName
	req.Auth.Scope.Project.Domain = d

	return json.Marshal(req)
}
func getToken(cfg *apiConfig) (string, error) {
	client := discoveryutils.GetHTTPClient()

	//	logger.Infof("req send: %s", string(cfg.authTokenReq))
	resp, err := client.Post(cfg.endpoint+"/auth/tokens", "application/json", bytes.NewBuffer(cfg.authTokenReq))
	if err != nil {
		return "", err
	}
	r, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	at := resp.Header.Get("X-Subject-Token")
	//	logger.Infof("get cod: %d, token: %s\n\n%s\n\n", resp.StatusCode, at, string(r))
	aur := AuthResp{}
	err = json.Unmarshal(r, &aur)
	if err != nil {
		return "", err
	}
	// TODO move nova detection somewhere else
	novaEndpoint := aur.novaEndpoint(cfg.availability, cfg.region)
	//	logger.Infof("parsed: %v", novaEndpoint)
	if novaEndpoint == nil {
		return "", errors.New("Cannot get novaEndpoint, not enough permissions?")
	}
	cfg.novaEndpoint = novaEndpoint.URL

	return at, nil
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
