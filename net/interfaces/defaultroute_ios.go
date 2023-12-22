// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build ios

package interfaces

import (
	"log"
)

func defaultRoute() (d DefaultRouteDetails, err error) {
	// We cannot rely on the delegated interface data on iOS. The NetworkExtension framework
	// seems to set the delegate interface only once, upon the *creation* of the VPN tunnel.
	// If a network transition (e.g. from Wi-Fi to Cellular) happens while the tunnel is
	// connected, it will be ignored and we will still try to set Wi-Fi as the default route
	// because the delegated interface is not updated by the NetworkExtension framework.
	//
	// Here we special-case iPhones and iPads with a simpler logic that doesn't look at
	// the routing table: we try finding a hardcoded Wi-Fi interface, if it doesn't have
	// an address, we fall back to cellular.

	// Start by getting all available interfaces.
	interfaces, err := netInterfaces()
	if err != nil {
		log.Printf("defaultroute_ios: could not get interfaces: %v", err)
		return d, ErrNoGatewayIndexFound
	}

	// We start by looking at the Wi-Fi interface, which on iPhone is always called en0.
	for _, ifc := range interfaces {
		if ifc.Name != "en0" {
			continue
		}

		if !ifc.IsUp() {
			log.Println("defaultroute_ios: Wi-Fi interface en0 isn't up, will try cellular")
			break
		}

		addrs, _ := ifc.Addrs()
		if len(addrs) == 0 {
			log.Println("defaultroute_ios: Wi-Fi interface en0 has no addresses")
			break
		}

		log.Println("defaultroute_ios: returning Wi-Fi interface en0")
		d.InterfaceName = ifc.Name
		d.InterfaceIndex = ifc.Index
		return d, nil
	}

	// Did it not work? Let's try with Cellular (pdp_ip0).
	for _, ifc := range interfaces {
		if ifc.Name != "pdp_ip0" {
			continue
		}

		if !ifc.IsUp() {
			log.Println("defaultroute_ios: cellular interface pdp_ip0 isn't up")
			break
		}

		addrs, _ := ifc.Addrs()
		if len(addrs) == 0 {
			log.Println("defaultroute_ios: cellular interface pdp_ip0 has no addresses")
			break
		}

		log.Println("defaultroute_ios: returning cellular interface pdp_ip0")
		d.InterfaceName = ifc.Name
		d.InterfaceIndex = ifc.Index
		return d, nil
	}

	return d, ErrNoGatewayIndexFound
}
