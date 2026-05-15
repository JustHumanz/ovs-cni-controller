package neutronOperator

import (
	"context"
	"fmt"
	"net"

	openstackv1 "github.com/JustHumanz/ovs-cni-controller/api/v1"
	"github.com/JustHumanz/ovs-cni-controller/internal/utils"
	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
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

func hasIP(prt ports.Port, ip string) bool {
	for _, fixedIP := range prt.FixedIPs {
		if fixedIP.IPAddress == ip {
			return true
		}
	}
	return false
}

func NeutronInit(neutronConf *openstackv1.NeutronConfig, auth OSAuthConfig, l logr.Logger) (*gophercloud.ServiceClient, error) {
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

	l.Info("Successfully parsed OpenStack auth config", "authURL", auth.Clouds.Openstack.Auth.AuthURL)

	network, err := networks.Get(context.Background(), neutronclient, neutronConf.Spec.NetworkUUID).Extract()
	if err != nil {
		return nil, err
	}

	l.Info("Successfully fetched network from OpenStack", "networkID", network.ID, "networkName", network.Name)

	return neutronclient, nil

}

func NeutronCreatePorts(neutronConf *openstackv1.NeutronConfig, client *gophercloud.ServiceClient, l logr.Logger) ([]*ports.Port, error) {
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
OuterLoop:
	for _, ip := range utils.PrintIPList(neutronConf.Spec.Ips) {
		for _, prt := range existingPorts {
			if hasIP(prt, ip) {
				continue OuterLoop
			}
		}

		l.Info("Creating port with IP", "ip", ip)
		prt, err := ports.Create(context.TODO(), client, ports.CreateOpts{
			NetworkID:   neutronConf.Spec.NetworkUUID,
			FixedIPs:    []ports.IP{{IPAddress: ip}},
			DeviceOwner: "network:kubePods",
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("failed to create port for IP %s: %v", ip, err)
		}

		prtSubnet, err := subnets.Get(context.Background(), client, prt.FixedIPs[0].SubnetID).Extract()
		if err != nil {
			return nil, fmt.Errorf("failed to fetch subnet for port %s: %v", prt.ID, err)
		}

		_, ipnet, err := net.ParseCIDR(prtSubnet.CIDR)
		if err != nil {
			return nil, fmt.Errorf("failed to parse subnet CIDR: %v", err)
		}

		gwIp := "None"
		if prtSubnet.GatewayIP != "" {
			gwIp = prtSubnet.GatewayIP
		}
		prt.Tags = append(prt.Tags, fmt.Sprintf("subnet=%s", net.IP(ipnet.Mask).String()))
		prt.Tags = append(prt.Tags, fmt.Sprintf("gateway=%s", gwIp))

		prt_res = append(prt_res, prt)
		l.Info("Successfully created port", "portID", prt.ID, "ipAddr", prt.FixedIPs[0].IPAddress)

	}

	return prt_res, nil
}

func NeutronDeletePort(PortID string, client *gophercloud.ServiceClient) error {
	return ports.Delete(context.TODO(), client, PortID).ExtractErr()
}

func NeutronGetPort(PortID string, client *gophercloud.ServiceClient) (*ports.Port, error) {
	return ports.Get(context.TODO(), client, PortID).Extract()
}

func NeutronUpdatePort(PortID string, client *gophercloud.ServiceClient, opts ports.UpdateOpts) (*ports.Port, error) {
	return ports.Update(context.TODO(), client, PortID, opts).Extract()
}
