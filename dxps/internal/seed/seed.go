// Package seed holds the reference tenants and NE inventory for the local DxPS environment.
package seed

import (
	"fmt"

	"dxps/internal/registry"
	"dxps/internal/tenant"
)

func Tenants() []tenant.Tenant {
	return []tenant.Tenant{
		{ID: "host-mno", Name: "Host MNO", Type: tenant.Host, SLATier: "GOLD", SLAWeight: 8, QuotaTPS: 2000, Status: tenant.Active},
		{ID: "mvno-alpha", Name: "MVNO Alpha (full MVNO, own OCS)", Type: tenant.FullMVNO, Host: "host-mno", SLATier: "SILVER", SLAWeight: 4, QuotaTPS: 500, Status: tenant.Active},
		{ID: "mvno-beta", Name: "MVNO Beta (light MVNO)", Type: tenant.LightMVNO, Host: "host-mno", SLATier: "BRONZE", SLAWeight: 2, QuotaTPS: 200, Status: tenant.Active},
		{ID: "ent-acme", Name: "ACME Corp (enterprise)", Type: tenant.Enterprise, Host: "host-mno", SLATier: "BRONZE", SLAWeight: 2, QuotaTPS: 100, Status: tenant.Active},
		{ID: "mvno-gamma", Name: "MVNO Gamma (suspended)", Type: tenant.LightMVNO, Host: "host-mno", SLATier: "BRONZE", SLAWeight: 1, QuotaTPS: 50, Status: tenant.Suspended},
	}
}

// Simulator ports (netsim).
const (
	PortNRF      = 9101
	PortUDR      = 9102
	PortIMS      = 9103
	PortRESTCONF = 9104
	PortSMDP     = 9105
	PortOCS      = 9106
	PortNPDB     = 9107
	PortNEF      = 9108
	PortHubSink  = 9109
	PortAdmin    = 9199
)

type NE struct {
	registry.NE
	Port   int
	Access []registry.Access
}

func NEs(host string) []NE {
	ep := func(port int, typ string) []registry.Endpoint {
		return []registry.Endpoint{{AdapterType: typ, BaseURI: fmt.Sprintf("https://%s:%d", host, port), Weight: 100}}
	}
	mk := func(ten, code, domain, nf, vendor string, port, tps int, shared bool, acc ...registry.Access) NE {
		return NE{NE: registry.NE{Tenant: ten, Code: code, Domain: domain, NFType: nf, Vendor: vendor, Shared: shared,
			MaxTPS: tps, MaxConc: 64, ReservedPct: 20, Endpoints: ep(port, domain)}, Port: port, Access: acc}
	}
	acc := func(t string, tps int, group string, ops ...string) registry.Access {
		return registry.Access{Tenant: t, QuotaTPS: tps, AllowedOps: ops, SubscriberGroup: group}
	}
	return []NE{
		mk("host-mno", "nrf01", "sba", "NRF", "3GPP", PortNRF, 2000, true),
		mk("host-mno", "udr01", "sba", "UDR", "3GPP-R18", PortUDR, 1500, true,
			acc("mvno-alpha", 400, "mvno-alpha", "udr.*"), acc("mvno-beta", 150, "mvno-beta", "udr.*")),
		mk("host-mno", "imspg01", "ims", "IMS_PG", "IMS-PG", PortIMS, 800, true,
			acc("mvno-alpha", 200, "", "ims.*"), acc("mvno-beta", 100, "", "ims.*")),
		mk("host-mno", "pe01", "netconf", "ROUTER", "IETF-L3SM", PortRESTCONF, 200, true,
			acc("ent-acme", 50, "", "netconf.*")),
		mk("host-mno", "olt01", "access", "OLT", "BBF-TR385", PortRESTCONF, 300, false),
		mk("host-mno", "usp01", "access", "USP_CTRL", "BBF-TR369", PortRESTCONF, 300, false),
		mk("host-mno", "smdp01", "esim", "SMDP", "GSMA-SGP22", PortSMDP, 300, true,
			acc("mvno-alpha", 100, "", "esim.*"), acc("mvno-beta", 50, "", "esim.*")),
		mk("host-mno", "ocs01", "bss", "OCS", "TMF666", PortOCS, 800, true,
			acc("mvno-beta", 150, "", "bss.*")),
		mk("mvno-alpha", "ocs-alpha", "bss", "OCS", "TMF666", PortOCS, 400, false),
		mk("host-mno", "npdb01", "bss", "NPDB", "NPDB", PortNPDB, 200, true,
			acc("mvno-alpha", 50, "", "npdb.*"), acc("mvno-beta", 50, "", "npdb.*")),
		mk("host-mno", "nef01", "exposure", "NEF", "CAMARA", PortNEF, 300, true,
			acc("mvno-alpha", 100, "", "camara.*", "nef.*"), acc("ent-acme", 50, "", "camara.*")),
	}
}
