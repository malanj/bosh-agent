package net

import (
	"bytes"
	"path/filepath"
	"strings"
	"text/template"

	bosherr "github.com/cloudfoundry/bosh-agent/errors"
	boshlog "github.com/cloudfoundry/bosh-agent/logger"
	bosharp "github.com/cloudfoundry/bosh-agent/platform/net/arp"
	boship "github.com/cloudfoundry/bosh-agent/platform/net/ip"
	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	boshsys "github.com/cloudfoundry/bosh-agent/system"
)

const centosNetManagerLogTag = "centosNetManager"

type centosNetManager struct {
	DefaultNetworkResolver

	fs                 boshsys.FileSystem
	cmdRunner          boshsys.CmdRunner
	routesSearcher     RoutesSearcher
	ipResolver         boship.Resolver
	addressBroadcaster bosharp.AddressBroadcaster
	logger             boshlog.Logger
}

func NewCentosNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	defaultNetworkResolver DefaultNetworkResolver,
	ipResolver boship.Resolver,
	addressBroadcaster bosharp.AddressBroadcaster,
	logger boshlog.Logger,
) centosNetManager {
	return centosNetManager{
		DefaultNetworkResolver: defaultNetworkResolver,
		fs:                 fs,
		cmdRunner:          cmdRunner,
		ipResolver:         ipResolver,
		addressBroadcaster: addressBroadcaster,
		logger:             logger,
	}
}

func (net centosNetManager) SetupDhcp(networks boshsettings.Networks, errCh chan error) error {
	net.logger.Debug(centosNetManagerLogTag, "Configuring DHCP networking")

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(centosDHCPConfigTemplate))

	// Keep DNS servers in the order specified by the network
	// because they are added by a *single* DHCP's prepend command
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	dnsServersList := strings.Join(dnsNetwork.DNS, ", ")
	err := t.Execute(buffer, dnsServersList)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	written, err := net.fs.ConvergeFileContents("/etc/dhcp/dhclient.conf", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/dhcp/dhclient.conf")
	}

	if written {
		net.restartNetwork()
	}

	addresses := []boship.InterfaceAddress{
		// eth0 is hard coded in AWS and OpenStack stemcells.
		// TODO: abstract hardcoded network interface name to the Manager
		boship.NewResolvingInterfaceAddress("eth0", net.ipResolver),
	}

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return err
}

// DHCP Config file - /etc/dhcp3/dhclient.conf
const centosDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;
{{ if . }}
prepend domain-name-servers {{ . }};{{ end }}
`

func (net centosNetManager) SetupManualNetworking(networks boshsettings.Networks, errCh chan error) error {
	net.logger.Debug(centosNetManagerLogTag, "Configuring manual networking")

	modifiedNetworks, err := net.writeIfcfgs(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing network interfaces")
	}

	net.restartNetwork()

	err = net.writeResolvConf(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing resolv.conf")
	}

	addresses := toInterfaceAddresses(modifiedNetworks)

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return nil
}

func (net centosNetManager) writeIfcfgs(networks boshsettings.Networks) ([]customNetwork, error) {
	var modifiedNetworks []customNetwork

	macAddresses, err := net.detectMacAddresses()
	if err != nil {
		return modifiedNetworks, bosherr.WrapError(err, "Detecting mac addresses")
	}

	gatewayNetwork, gatewayNetworkFound := networks.DefaultNetworkFor("gateway")

	if !gatewayNetworkFound {
		return modifiedNetworks, bosherr.WrapError(err, "Finding network for default gateway")
	}

	for _, aNet := range networks {
		var network, broadcast string
		network, broadcast, err = boshsys.CalculateNetworkAndBroadcast(aNet.IP, aNet.Netmask)
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Calculating network and broadcast")
		}

		newNet := customNetwork{
			aNet,
			macAddresses[aNet.Mac],
			network,
			broadcast,
			aNet.IP == gatewayNetwork.IP,
		}
		modifiedNetworks = append(modifiedNetworks, newNet)

		buffer := bytes.NewBuffer([]byte{})
		t := template.Must(template.New("ifcfg").Parse(centosIfcgfTemplate))

		err = t.Execute(buffer, newNet)
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Generating config from template")
		}

		err = net.fs.WriteFile(filepath.Join("/etc/sysconfig/network-scripts", "ifcfg-"+newNet.Interface), buffer.Bytes())
		if err != nil {
			return modifiedNetworks, bosherr.WrapError(err, "Writing to /etc/sysconfig/network-scripts")
		}
	}

	return modifiedNetworks, nil
}

const centosIfcgfTemplate = `DEVICE={{ .Interface }}
BOOTPROTO=static
IPADDR={{ .IP }}
NETMASK={{ .Netmask }}
BROADCAST={{ .Broadcast }}
{{ if .HasDefaultGateway }}GATEWAY={{ .Gateway }}{{ end }}
ONBOOT=yes`

func (net centosNetManager) writeResolvConf(networks boshsettings.Networks) error {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("resolv-conf").Parse(centosResolvConfTemplate))

	// Keep DNS servers in the order specified by the network
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	dnsServersArg := dnsConfigArg{dnsNetwork.DNS}

	err := t.Execute(buffer, dnsServersArg)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	err = net.fs.WriteFile("/etc/resolv.conf", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/resolv.conf")
	}

	return nil
}

const centosResolvConfTemplate = `# Generated by bosh-agent
{{ range .DNSServers }}nameserver {{ . }}
{{ end }}`

func (net centosNetManager) detectMacAddresses() (map[string]string, error) {
	addresses := map[string]string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		return addresses, bosherr.WrapError(err, "Getting file list from /sys/class/net")
	}

	var macAddress string
	for _, filePath := range filePaths {
		macAddress, err = net.fs.ReadFileString(filepath.Join(filePath, "address"))
		if err != nil {
			return addresses, bosherr.WrapError(err, "Reading mac address from file")
		}

		macAddress = strings.Trim(macAddress, "\n")

		interfaceName := filepath.Base(filePath)
		addresses[macAddress] = interfaceName
	}

	return addresses, nil
}

func (net centosNetManager) restartNetwork() {
	net.logger.Debug(centosNetManagerLogTag, "Restarting networking")

	_, _, _, err := net.cmdRunner.RunCommand("service", "network", "restart")
	if err != nil {
		net.logger.Error(centosNetManagerLogTag, "Ignoring network restart failure: %#v", err)
	}
}
