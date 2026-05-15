package utils

import (
	"log"
	"net"
	"strings"
)

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

func PrintIPList(IPs []string) []string {
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

func BoolPtr(b bool) *bool { return &b }

func StringPtr(s string) *string { return &s }
