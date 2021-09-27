/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstackipset

import (
	"fmt"
	"net"

	"strconv"
	"strings"

	ospdirectorv1beta1 "github.com/openstack-k8s-operators/osp-director-operator/api/v1beta1"
	common "github.com/openstack-k8s-operators/osp-director-operator/pkg/common"
)

type networkType struct {
	Name            string
	NameLower       string
	Cidr            string // e.g. 192.168.24.0/24
	NetAddr         string // e.g. 192.168.24.0
	CidrSuffix      int    // e.g. 24
	MTU             int
	AllocationStart string
	AllocationEnd   string
	Gateway         string
	Vlan            int
}

// information to build NodePortMap entry:
//   ip_address: 192.168.24.9 (2001:DB8:24::9)
//   ip_subnet: 192.168.24.9/24 (2001:DB8:24::9/64)
//   ip_address_uri: 192.168.24.9 ([2001:DB8:24::9])
type ipType struct {
	IPaddr       string // e.g. 192.168.24.9
	IPAddrURI    string // e.g. 192.168.24.9
	IPAddrSubnet string // e.g. 192.168.24.9/24
	Subnet       string // e.g. 192.168.24.0/24
}

type roleType struct {
	Name      string
	NameLower string
	Networks  map[string]*networkType
	Nodes     map[string]*nodeType
}

type nodeType struct {
	Index                   int
	IPaddr                  map[string]*ipType
	Hostname                string
	VIP                     bool
	OVNStaticBridgeMappings map[string]string
}

func getCidrParts(cidr string) (string, int, error) {
	cidrPieces := strings.Split(cidr, "/")
	cidrSuffix, err := strconv.Atoi(cidrPieces[len(cidrPieces)-1])
	if err != nil {
		return "", cidrSuffix, err
	}

	return cidrPieces[0], cidrSuffix, nil
}

// CreateConfigMapParams - creates a map of parameters for the overcloud ipset config map
func CreateConfigMapParams(overcloudNetList ospdirectorv1beta1.OpenStackNetList, overcloudMACList ospdirectorv1beta1.OpenStackMACAddressList) (map[string]interface{}, error) {

	templateParameters := make(map[string]interface{})

	// DeployedServerPortMap:
	// https://docs.openstack.org/project-deploy-guide/tripleo-docs/latest/features/custom_networks.html
	// https://docs.openstack.org/project-deploy-guide/tripleo-docs/latest/features/deployed_server.html
	// https://specs.openstack.org/openstack/tripleo-specs/specs/wallaby/triplo-network-data-v2-node-ports.html

	// map with details for all networks
	networksMap := map[string]*networkType{}
	rolesMap := map[string]*roleType{}
	var osnetName string

	for _, osnet := range overcloudNetList.Items {

		// CR names won't allow '_', need to change tripleo nets using those
		switch osnet.Name {
		case "internalapi":
			osnetName = InternalAPIName
		case "storagemgmt":
			osnetName = StorageMgmtName
		default:
			osnetName = osnet.Name
		}

		// create map of all network
		if networksMap[osnetName] == nil {
			netAddr, cidrSuffix, err := getCidrParts(osnet.Spec.Cidr)
			if err != nil {
				return templateParameters, err
			}
			networksMap[osnetName] = &networkType{
				Name:            GetNetName(osnetName),
				NameLower:       osnetName,
				Cidr:            osnet.Spec.Cidr,
				CidrSuffix:      cidrSuffix,
				NetAddr:         netAddr,
				MTU:             1500, //TODO custom MTU per network
				AllocationStart: osnet.Spec.AllocationStart,
				AllocationEnd:   osnet.Spec.AllocationEnd,
				Gateway:         osnet.Spec.Gateway,
				Vlan:            osnet.Spec.Vlan,
			}
		}

		for roleName, roleReservation := range osnet.Status.RoleReservations {
			if !roleReservation.AddToPredictableIPs {
				continue
			}

			// create map of all roles with Name and Count
			if rolesMap[roleName] == nil {
				rolesMap[roleName] = &roleType{
					Name:      roleName,
					NameLower: strings.ToLower(roleName),
					Networks:  map[string]*networkType{},
					Nodes:     map[string]*nodeType{},
				}
			}

			// add network details to role
			if rolesMap[roleName].Networks[osnetName] == nil {
				rolesMap[roleName].Networks[osnetName] = networksMap[osnetName]
			}

			hostnameMapIndex := 0
			for _, reservation := range roleReservation.Reservations {

				ovnStaticBridgeMappings := map[string]string{}
				// get OVNStaticBridgeMacMappings information from overcloudMACList
				for _, macReservations := range overcloudMACList.Items {
					for node, macReservation := range macReservations.Status.MACReservations {
						if node == reservation.Hostname {
							for net, mac := range macReservation.Reservations {
								ovnStaticBridgeMappings[net] = mac
							}
						}
					}
				}

				if !reservation.Deleted {
					if rolesMap[roleName].Nodes[reservation.Hostname] == nil {
						rolesMap[roleName].Nodes[reservation.Hostname] = &nodeType{
							Index:                   hostnameMapIndex,
							IPaddr:                  map[string]*ipType{},
							Hostname:                reservation.Hostname,
							VIP:                     reservation.VIP,
							OVNStaticBridgeMappings: ovnStaticBridgeMappings,
						}
					}

					uri := reservation.IP
					if common.IsIPv6(net.ParseIP(reservation.IP)) {
						// IP address with brackets in case of IPv6, e.g. [2001:DB8:24::15]
						uri = fmt.Sprintf("[%s]", uri)
					}
					if rolesMap[roleName].Nodes[reservation.Hostname].IPaddr[osnetName] == nil {
						rolesMap[roleName].Nodes[reservation.Hostname].IPaddr[osnetName] = &ipType{
							IPaddr:       reservation.IP,
							IPAddrURI:    uri,
							IPAddrSubnet: fmt.Sprintf("%s/%d", reservation.IP, networksMap[osnetName].CidrSuffix),
							Subnet:       networksMap[osnetName].Cidr,
						}
					}
					hostnameMapIndex++
				}
			}
		}
	}

	templateParameters["RolesMap"] = rolesMap
	templateParameters["NetworksMap"] = networksMap

	return templateParameters, nil

}

// GetNetNameLower -
func GetNetNameLower(net string) string {
	return strings.ToLower(networkDict()(net))
}

// GetNetName -
func GetNetName(net string) string {
	return networkDict()(net)
}

func networkDict() func(string) string {
	// innerMap is captured in the closure returned below
	innerMap := map[string]string{
		"ctlplane":     "Control",
		"internal_api": "InternalApi",
		"external":     "External",
		"storage":      "Storage",
		"storage_mgmt": "StorageMgmt",
		"tenant":       "Tenant",
		"management":   "Management",
	}

	return func(key string) string {
		return innerMap[key]
	}
}
