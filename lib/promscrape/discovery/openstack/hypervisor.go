package openstack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

/*
{
    "hypervisors": [
        {
            "cpu_info": {
                "arch": "x86_64",
                "model": "Nehalem",
                "vendor": "Intel",
                "features": [
                    "pge",
                    "clflush"
                ],
                "topology": {
                    "cores": 1,
                    "threads": 1,
                    "sockets": 4
                }
            },
            "current_workload": 0,
            "status": "enabled",
            "state": "up",
            "disk_available_least": 0,
            "host_ip": "1.1.1.1",
            "free_disk_gb": 1028,
            "free_ram_mb": 7680,
            "hypervisor_hostname": "host1",
            "hypervisor_type": "fake",
            "hypervisor_version": 1000,
            "id": 2,
            "local_gb": 1028,
            "local_gb_used": 0,
            "memory_mb": 8192,
            "memory_mb_used": 512,
            "running_vms": 0,
            "service": {
                "host": "host1",
                "id": 6,
                "disabled_reason": null
            },
            "vcpus": 2,
            "vcpus_used": 0
        }
    ],
    "hypervisors_links": [
        {
            "href": "http://openstack.example.com/v2.1/6f70656e737461636b20342065766572/os-hypervisors/detail?limit=1&marker=2",
            "rel": "next"
        }
    ]
}

*/

type hypervisor struct {
	HostIP   string `json:"host_ip"`
	ID       int    `json:"id"`
	Hostname string `json:"hypervisor_hostname"`
	Status   string `json:"status"`
	State    string `json:"state"`
	Type     string `json:"hypervisor_type"`
}

type hypervisorDetail struct {
	Hypervisors []hypervisor `json:"hypervisors"`
	Links       []struct {
		HREF string `json:"href"`
		Rel  string `json:"rel"`
	} `json:"hypervisors_links"`
}

func hypervisorAPIResponse(href string, cfg *apiConfig) ([]byte, error) {
	token, err := cfg.getFreshAPICredentials()
	req, err := http.NewRequest("GET", href, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create new request for openstach hvs discovery: %w", err)
	}
	req.Header.Set(authHearName, token.token)
	resp, err := cfg.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed query openstack api for hypervisor details: %w", err)
	}
	return readResponseBody(resp, href)

}

func parseHypervisorDetail(data []byte) (*hypervisorDetail, error) {
	var hvsd hypervisorDetail
	if err := json.Unmarshal(data, &hvsd); err != nil {
		return nil, err
	}
	return &hvsd, nil
}

func (cfg *apiConfig) getHypervisors() ([]hypervisor, error) {
	novaURL := *cfg.creds.computeURL
	novaURL.Path = path.Join(novaURL.Path, "os-hypervisors", "detail")
	nextLink := novaURL.String()
	var hvs []hypervisor
	for {
		resp, err := hypervisorAPIResponse(nextLink, cfg)
		if err != nil {
			return nil, err
		}

		detail, err := parseHypervisorDetail(resp)
		if err != nil {
			return nil, err
		}
		hvs = append(hvs, detail.Hypervisors...)

		if len(detail.Links) > 0 {
			nextLink = detail.Links[0].HREF
			continue
		}
		return hvs, nil
	}
}

func addHypervisorLabels(ms []map[string]string, hvs []hypervisor, port int) []map[string]string {
	for _, hv := range hvs {
		addr := discoveryutils.JoinHostPort(hv.HostIP, port)
		m := map[string]string{
			"__address__":                          addr,
			"__meta_openstack_hypervisor_type":     hv.Type,
			"__meta_openstack_hypervisor_status":   hv.Status,
			"__meta_openstack_hypervisor_hostname": hv.Hostname,
			"__meta_openstack_hypervisor_state":    hv.State,
			"__meta_openstack_hypervisor_host_ip":  hv.HostIP,
			"__meta_openstack_hypervisor_id":       strconv.Itoa(hv.ID),
		}
		ms = append(ms, m)

	}
	return ms
}

func getHypervisorLabels(cfg *apiConfig) ([]map[string]string, error) {
	hvs, err := cfg.getHypervisors()
	if err != nil {
		return nil, fmt.Errorf("cannot get hypervisors: %w", err)
	}
	var ms []map[string]string
	return addHypervisorLabels(ms, hvs, cfg.port), nil

}
