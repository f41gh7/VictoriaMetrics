package openstack

import (
	"encoding/json"
	"path"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

type instance struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	HostID    string `json:"hostid"`
	Status    string `json:"status"`
	Addresses map[string][]struct {
		Address string `json:"addr"`
		Version int    `json:"version"`
		Type    string `json:"OS-EXT-IPS:type"`
	} `json:"addresses"`
	Metadata map[string]string `json:"metadata"`
	Flavor   struct {
		ID string `json:"id"`
	} `json:"flavor"`
}

// https://docs.openstack.org/api-ref/compute/?expanded=list-servers-detailed-detail#list-servers
type serversDetail struct {
	Servers []instance `json:"servers"`
	Links   []struct {
		HREF string `json:"href"`
		Rel  string `json:"rel"`
	} `json:"servers_links"`
}

func parseServersDetail(data []byte) (*serversDetail, error) {
	var srvd serversDetail
	if err := json.Unmarshal(data, &srvd); err != nil {
		return nil, err
	}
	return &srvd, nil
}

func addInstanceLabels(ms []map[string]string, servers []instance, port int) []map[string]string {
	for _, server := range servers {
		m := map[string]string{
			"__meta_openstack_instance_id":     server.ID,
			"__meta_openstack_instance_status": server.Status,
			"__meta_openstack_instance_name":   server.Name,
			"__meta_openstack_project_id":      server.TenantID,
			"__meta_openstack_user_id":         server.UserID,
			"__meta_openstack_instance_flavor": server.Flavor.ID,
		}

		for k, v := range server.Metadata {
			m["__meta_openstack_tag_"+discoveryutils.SanitizeLabelName(k)] = v
		}
		for pool, addresses := range server.Addresses {
			if len(addresses) == 0 {
				// pool with zero addresses skip it
				continue
			}
			var publicIP string
			// its possible to have only one floating ip per pool
			for _, ip := range addresses {
				if ip.Type != "floating" {
					continue
				}
				publicIP = ip.Address
				break
			}
			for _, ip := range addresses {
				// fast return
				if len(ip.Address) == 0 || ip.Type == "floating" {
					continue
				}
				// copy labels
				lbls := make(map[string]string, len(m))
				for k, v := range m {
					lbls[k] = v
				}
				lbls["__meta_openstack_address_pool"] = pool
				lbls["__meta_openstack_private_ip"] = ip.Address
				if len(publicIP) > 0 {
					lbls["__meta_openstack_public_ip"] = publicIP
				}
				lbls["__address__"] = discoveryutils.JoinHostPort(ip.Address, port)
				ms = append(ms, lbls)

			}
		}
	}
	return ms
}

func (cfg *apiConfig) getServers() ([]instance, error) {
	novaURL := *cfg.creds.computeURL
	novaURL.Path = path.Join(novaURL.Path, "servers", "detail")
	if !cfg.allTenants {
		q := novaURL.Query()
		q.Set("all_tenants", "false")
		novaURL.RawQuery = q.Encode()
	}

	nextLink := novaURL.String()

	var servers []instance
	for {
		resp, err := getAPIResponse(nextLink, cfg)
		if err != nil {
			return nil, err
		}

		serversDetail, err := parseServersDetail(resp)
		if err != nil {
			return nil, err
		}
		servers = append(servers, serversDetail.Servers...)

		if len(serversDetail.Links) > 0 {
			nextLink = serversDetail.Links[0].HREF
			continue
		}
		return servers, nil
	}
}

func getInstancesLabels(cfg *apiConfig) ([]map[string]string, error) {
	srv, err := cfg.getServers()
	if err != nil {
		return nil, err
	}
	var ms []map[string]string
	ms = addInstanceLabels(ms, srv, cfg.port)
	return ms, nil
}
