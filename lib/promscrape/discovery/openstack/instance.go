package openstack

import (
	"encoding/json"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

type instance struct {
	ID string `json:"id"`

	// TenantID identifies the tenant owning this server resource.
	TenantID string `json:"tenant_id"`

	// UserID uniquely identifies the user account owning the tenant.
	UserID string `json:"user_id"`

	// Name contains the human-readable name for the server.
	Name string `json:"name"`
	// HostID is the host where the server is located in the cloud.
	HostID string `json:"hostid"`

	// Status contains the current operational status of the server,
	// such as IN_PROGRESS or ACTIVE.
	Status string `json:"status"`

	// AccessIPv4 and AccessIPv6 contain the IP addresses of the server,
	// suitable for remote access for administration.
	AccessIPv4 string `json:"accessIPv4"`
	AccessIPv6 string `json:"accessIPv6"`
	Addresses  map[string][]struct {
		Address string `json:"addr"`
		Version int    `json:"version"`
		Type    string `json:"OS-EXT-IPS:type"`
	} `json:"addresses"`
	HostId     string            `json:"host_id"`
	HostStatus string            `json:"host_status"`
	Metadata   map[string]string `json:"metadata"`
	Flavor     struct {
		ID string `json:"id"`
	} `json:"flavor"`
}

func (cfg *apiConfig) getServers() ([]instance, error) {
	nextLink := cfg.novaEndpoint + "/servers/detail"
	if !cfg.allTenants {
		nextLink += "?all_tenants=false"
	}
	var servers []instance
	for {
		resp, err := hypervisorAPIResponse(nextLink, cfg)
		if err != nil {
			return nil, err
		}

		logger.Infof("get resp %s", string(resp))
		detail, err := parseServersDetail(resp)
		if err != nil {
			return nil, err
		}
		servers = append(servers, detail.Servers...)

		if len(detail.Links) > 0 {
			nextLink = detail.Links[0].HREF
			continue
		}
		return servers, nil
	}
}

type serversDetail struct {
	Servers []instance
	Links   []struct {
		HREF string
		Rel  string
	}
}

func parseServersDetail(data []byte) (*serversDetail, error) {
	srvd := serversDetail{}
	if err := json.Unmarshal(data, &srvd); err != nil {
		return nil, err
	}
	return &srvd, nil
}

//
func getInstancesLabels(cfg *apiConfig) ([]map[string]string, error) {
	srv, err := cfg.getServers()
	if err != nil {
		return nil, err
	}
	var ms []map[string]string
	ms = addInstanceLabels(ms, srv, cfg.port)
	return ms, nil
}

func addInstanceLabels(ms []map[string]string, servers []instance, port int) []map[string]string {
	for _, server := range servers {
		/*
			openstackLabelInstanceFlavor = openstackLabelPrefix + "instance_flavor"
			openstackLabelInstanceID     = openstackLabelPrefix + "instance_id"
			openstackLabelInstanceName   = openstackLabelPrefix + "instance_name"
			openstackLabelInstanceStatus = openstackLabelPrefix + "instance_status"
			openstackLabelPrivateIP      = openstackLabelPrefix + "private_ip"
			openstackLabelProjectID      = openstackLabelPrefix + "project_id"
			openstackLabelPublicIP       = openstackLabelPrefix + "public_ip"
			openstackLabelTagPrefix      = openstackLabelPrefix + "tag_"
			openstackLabelUserID         = openstackLabelPrefix + "user_id"

		*/
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

/*
	openstackLabelPrefix         = model.MetaLabelPrefix + "openstack_"
	openstackLabelAddressPool    = openstackLabelPrefix + "address_pool"
	openstackLabelInstanceFlavor = openstackLabelPrefix + "instance_flavor"
	openstackLabelInstanceID     = openstackLabelPrefix + "instance_id"
	openstackLabelInstanceName   = openstackLabelPrefix + "instance_name"
	openstackLabelInstanceStatus = openstackLabelPrefix + "instance_status"
	openstackLabelPrivateIP      = openstackLabelPrefix + "private_ip"
	openstackLabelProjectID      = openstackLabelPrefix + "project_id"
	openstackLabelPublicIP       = openstackLabelPrefix + "public_ip"
	openstackLabelTagPrefix      = openstackLabelPrefix + "tag_"
	openstackLabelUserID         = openstackLabelPrefix + "user_id"


*/
