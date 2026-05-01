package controller

import (
	"context"
	"log"
	"net"
	"strings"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	openstackv1 "humanz.moe/kube-ovs/api/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Openstack Auth ConfigMap structure
type OSAuthConfig struct {
	Clouds struct {
		Openstack struct {
			Auth struct {
				AuthURL        string `yaml:"auth_url"`
				Username       string `yaml:"username"`
				Password       string `yaml:"password"`
				ProjectName    string `yaml:"project_name"`
				UserDomainName string `yaml:"user_domain_name"`
			} `yaml:"auth"`
			RegionName         string `yaml:"region_name"`
			Interface          string `yaml:"interface"`
			IdentityAPIVersion int    `yaml:"identity_api_version"`
		} `yaml:"openstack"`
	} `yaml:"clouds"`
}

func (r *NeutronConfigReconciler) NeutronInit(neutronConf *openstackv1.NeutronConfig, auth OSAuthConfig, l logr.Logger) (*gophercloud.ServiceClient, error) {
	providerClient, err := openstack.AuthenticatedClient(context.Background(), gophercloud.AuthOptions{
		IdentityEndpoint: auth.Clouds.Openstack.Auth.AuthURL,
		Username:         auth.Clouds.Openstack.Auth.Username,
		Password:         auth.Clouds.Openstack.Auth.Password,
		DomainName:       auth.Clouds.Openstack.Auth.UserDomainName,
		TenantName:       auth.Clouds.Openstack.Auth.ProjectName,
	})
	if err != nil {
		return nil, err

	}

	neutronclient, err := openstack.NewNetworkV2(providerClient, gophercloud.EndpointOpts{
		Region: auth.Clouds.Openstack.RegionName,
	})
	if err != nil {
		return nil, err
	}

	network, err := networks.Get(context.Background(), neutronclient, neutronConf.Spec.NetworkUUID).Extract()
	if err != nil {
		return nil, err
	}

	logf.Log.Info("Successfully fetched network from OpenStack", "networkID", network.ID, "networkName", network.Name)

	l.Info("Successfully parsed OpenStack auth config", "authURL", auth.Clouds.Openstack.Auth.AuthURL)
	return neutronclient, nil

}

func hasIP(prt ports.Port, ip string) bool {
	for _, fixedIP := range prt.FixedIPs {
		if fixedIP.IPAddress == ip {
			return true
		}
	}
	return false
}

func (r *NeutronConfigReconciler) NeutronDeletePort(PortID string, client *gophercloud.ServiceClient, l logr.Logger) error {
	return ports.Delete(context.TODO(), client, PortID).ExtractErr()
}

func (r *NeutronConfigReconciler) NeutronCreatePorts(neutronConf *openstackv1.NeutronConfig, client *gophercloud.ServiceClient, l logr.Logger) ([]*ports.Port, error) {
	allPorts, err := ports.List(client, ports.ListOpts{
		NetworkID:   neutronConf.Spec.NetworkUUID,
		DeviceOwner: "network:kubePods",
	}).AllPages(context.Background())
	if err != nil {
		return nil, err
	}

	existingPorts, err := ports.ExtractPorts(allPorts)
	if err != nil {
		return nil, err
	}

	prt_res := []*ports.Port{}
	for _, ip := range printIPList(neutronConf.Spec.Ips) {
		found := false
		for _, prt := range existingPorts {
			if hasIP(prt, ip) {
				prt_res = append(prt_res, &prt)
				found = true
				l.Info("Port already exists", "portID", prt.ID, "ipAddr", ip)
				break
			}
		}
		if !found {
			logf.Log.Info("Creating port with IP", "ip", ip)
			prt, err := ports.Create(context.TODO(), client, ports.CreateOpts{
				NetworkID:    neutronConf.Spec.NetworkUUID,
				FixedIPs:     []ports.IP{ports.IP{IPAddress: ip}},
				DeviceOwner:  "network:kubePods",
				AdminStateUp: nil,
			}).Extract()
			if err != nil {
				return nil, err
			}
			prt_res = append(prt_res, prt)
			l.Info("Successfully created port", "portID", prt.ID, "ipAddr", prt.FixedIPs[0].IPAddress)
		}
	}

	return prt_res, nil
}

// Increment IP address
func incIP(ip net.IP) net.IP {
	ipv4 := ip.To4()
	out := make(net.IP, len(ipv4))
	copy(out, ipv4)
	for j := len(out) - 1; j >= 0; j-- {
		out[j]++
		if out[j] != 0 {
			break
		}
	}
	return out
}

func ipsInRange(start, end net.IP) []string {
	ips := []string{}
	for ip := start; !ip.Equal(end); ip = incIP(ip) {
		ips = append(ips, ip.String())
	}
	ips = append(ips, end.String()) // include end
	return ips
}

func printIPList(IPs []string) []string {
	sortedIPs := []string{}
	for _, item := range IPs {
		switch {
		case strings.Contains(item, ".."): // Range
			parts := strings.Split(item, "..")
			start := net.ParseIP(parts[0]).To4()
			end := net.ParseIP(parts[1]).To4()
			for _, ip := range ipsInRange(start, end) {
				sortedIPs = append(sortedIPs, ip)
			}
		case strings.Contains(item, "/"): // CIDR
			ip, ipnet, err := net.ParseCIDR(item)
			if err != nil {
				log.Println("Invalid CIDR:", item)
				continue
			}
			for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); ip = incIP(ip) {
				sortedIPs = append(sortedIPs, ip.String())
			}
		default: // single IP
			sortedIPs = append(sortedIPs, item)
		}
	}
	return sortedIPs
}
