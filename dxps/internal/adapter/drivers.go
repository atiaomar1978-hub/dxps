package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dxps/internal/contract"
	"dxps/internal/registry"
)

// ---------------------------------------------------------------- sba: 3GPP Nudr-dr (TS 29.504/29.505) with NRF OAuth2 (TS 29.510)

const plmn = "41677"

var sbaRoutes = map[string]route{
	"udr.authSubscription.put":    {http.MethodPut, "/nudr-dr/v2/subscription-data/{supi}/authentication-data/authentication-subscription", ""},
	"udr.authSubscription.delete": {http.MethodDelete, "/nudr-dr/v2/subscription-data/{supi}/authentication-data/authentication-subscription", ""},
	"udr.amData.put":              {http.MethodPut, "/nudr-dr/v2/subscription-data/{supi}/" + plmn + "/provisioned-data/am-data", ""},
	"udr.amData.patch":            {http.MethodPatch, "/nudr-dr/v2/subscription-data/{supi}/" + plmn + "/provisioned-data/am-data", "application/merge-patch+json"},
	"udr.amData.delete":           {http.MethodDelete, "/nudr-dr/v2/subscription-data/{supi}/" + plmn + "/provisioned-data/am-data", ""},
	"udr.smfSelection.put":        {http.MethodPut, "/nudr-dr/v2/subscription-data/{supi}/" + plmn + "/provisioned-data/smf-selection-subscription-data", ""},
	"udr.smfSelection.delete":     {http.MethodDelete, "/nudr-dr/v2/subscription-data/{supi}/" + plmn + "/provisioned-data/smf-selection-subscription-data", ""},
	"udr.policyData.put":          {http.MethodPut, "/nudr-dr/v2/policy-data/ues/{supi}/am-data", ""},
	"udr.policyData.delete":       {http.MethodDelete, "/nudr-dr/v2/policy-data/ues/{supi}/am-data", ""},
}

type sba struct{}

func (sba) Domain() string                                                       { return "sba" }
func (sba) Supports(op string) bool                                              { return supports(sbaRoutes, op) }
func (sba) Interpret(s int, b map[string]any) (contract.Outcome, string, string) { return ok2xx(s, b) }

func (sba) Build(ctx context.Context, a *Adapter, ne *registry.NE, t *contract.NeTask) (*Call, error) {
	tok, err := a.nrfToken(ctx, ne, "nudr-dr")
	if err != nil {
		return nil, retryableErr{fmt.Errorf("NRF token: %w", err)}
	}
	prio := map[contract.Priority]string{contract.P0: "1", contract.P1: "8", contract.P2: "16", contract.P3: "24"}[t.Priority]
	h := map[string]string{
		"Authorization":              "Bearer " + tok,
		"3gpp-Sbi-Message-Priority":  prio,
		"3gpp-Sbi-Correlation-Info":  "imsi-" + strings.TrimPrefix(fmt.Sprint(t.Params["supi"]), "imsi-"),
		"3gpp-Sbi-Originating-Nf-Id": "dxps-adapter-sba",
	}
	return buildRoute(sbaRoutes, t, h)
}

// nrfToken returns a cached OAuth2 client-credentials token from the NRF owned by the NE's tenant.
func (a *Adapter) nrfToken(ctx context.Context, ne *registry.NE, scope string) (string, error) {
	var nrf *registry.NE
	for _, n := range a.Registry.All() {
		if n.NFType == "NRF" && n.Tenant == ne.Tenant {
			nrf = n
			break
		}
	}
	if nrf == nil {
		return "", fmt.Errorf("no NRF for %s", ne.Tenant)
	}
	ep, err := nrf.Endpoint()
	if err != nil {
		return "", err
	}
	key := ep.BaseURI + "|" + scope
	a.mu.Lock()
	if tk, ok := a.tokens[key]; ok && a.now().Before(tk.exp) {
		a.mu.Unlock()
		return tk.value, nil
	}
	a.mu.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}, "nfInstanceId": {"7c1d6f2e-0000-4000-8000-00000000d0e5"},
		"nfType": {"AF"}, "targetNfType": {"UDR"}, "scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.BaseURI+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("NRF status %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("NRF token response invalid")
	}
	exp := a.now().Add(time.Duration(max(tr.ExpiresIn-30, 1)) * time.Second)
	a.mu.Lock()
	a.tokens[key] = token{tr.AccessToken, exp}
	a.mu.Unlock()
	return tr.AccessToken, nil
}

// ---------------------------------------------------------------- ims: IMS provisioning gateway (HSS/TAS)

var imsRoutes = map[string]route{
	"ims.impi.put":     {http.MethodPut, "/ims-pg/v1/subscribers/{supi}/impi", ""},
	"ims.impi.delete":  {http.MethodDelete, "/ims-pg/v1/subscribers/{supi}/impi", ""},
	"ims.impu.put":     {http.MethodPut, "/ims-pg/v1/subscribers/{supi}/impu", ""},
	"ims.impu.delete":  {http.MethodDelete, "/ims-pg/v1/subscribers/{supi}/impu", ""},
	"ims.mmtel.put":    {http.MethodPut, "/ims-pg/v1/subscribers/{supi}/mmtel", ""},
	"ims.mmtel.delete": {http.MethodDelete, "/ims-pg/v1/subscribers/{supi}/mmtel", ""},
}

type ims struct{}

func (ims) Domain() string                                                       { return "ims" }
func (ims) Supports(op string) bool                                              { return supports(imsRoutes, op) }
func (ims) Interpret(s int, b map[string]any) (contract.Outcome, string, string) { return ok2xx(s, b) }
func (ims) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(imsRoutes, t, nil)
}

// ---------------------------------------------------------------- netconf: RESTCONF (RFC 8040) + YANG-Patch (RFC 8072)

var netconfRoutes = map[string]route{
	"netconf.l3vpn.patch":  {http.MethodPatch, "/restconf/data/ietf-l3vpn-svc:l3vpn-svc", "application/yang-patch+json"},
	"netconf.l3vpn.delete": {http.MethodDelete, "/restconf/data/ietf-l3vpn-svc:l3vpn-svc/sites/site={siteId}", ""},
}

type netconf struct{}

func (netconf) Domain() string          { return "netconf" }
func (netconf) Supports(op string) bool { return supports(netconfRoutes, op) }
func (netconf) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(netconfRoutes, t, map[string]string{"Accept": "application/yang-data+json"})
}

// Interpret checks the yang-patch-status (RFC 8072 section 2.3).
func (netconf) Interpret(_ int, b map[string]any) (contract.Outcome, string, string) {
	return yangPatchStatus(b)
}

func yangPatchStatus(b map[string]any) (contract.Outcome, string, string) {
	st, ok := b["ietf-yang-patch:yang-patch-status"].(map[string]any)
	if !ok {
		return contract.Succeeded, "", ""
	}
	if _, ok := st["ok"]; ok {
		return contract.Succeeded, "", ""
	}
	return contract.Failed, "DXPS-3422:yang-patch", "YANG patch rejected"
}

// ---------------------------------------------------------------- access: OLT (BBF TR-385 via RESTCONF) + CPE (TR-369 USP)

var accessRoutes = map[string]route{
	"access.ont.patch":  {http.MethodPatch, "/restconf/data/bbf-xpon-onu-states:onus", "application/yang-patch+json"},
	"access.ont.delete": {http.MethodDelete, "/restconf/data/bbf-xpon-onu-states:onus/onu={ontSerial}", ""},
	"access.cpe.set":    {http.MethodPost, "/usp/v1/agents/{cpeEndpoint}/set", "application/vnd.bbf.usp.msg+json"},
}

type access struct{}

func (access) Domain() string          { return "access" }
func (access) Supports(op string) bool { return supports(accessRoutes, op) }
func (access) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(accessRoutes, t, nil)
}

// Interpret handles YANG-Patch status and USP SetResp operation failures.
func (access) Interpret(_ int, b map[string]any) (contract.Outcome, string, string) {
	if o, c, m := yangPatchStatus(b); o != contract.Succeeded {
		return o, c, m
	}
	if body, ok := b["body"].(map[string]any); ok {
		if e, ok := body["error"].(map[string]any); ok {
			return contract.Failed, fmt.Sprintf("DXPS-3USP:%v", e["errCode"]), fmt.Sprint(e["errMsg"])
		}
	}
	return contract.Succeeded, "", ""
}

// ---------------------------------------------------------------- esim: GSMA SGP.22 ES2+

var esimRoutes = map[string]route{
	"esim.downloadOrder": {http.MethodPost, "/gsma/rsp2/es2plus/downloadOrder", ""},
	"esim.confirmOrder":  {http.MethodPost, "/gsma/rsp2/es2plus/confirmOrder", ""},
	"esim.cancelOrder":   {http.MethodPost, "/gsma/rsp2/es2plus/cancelOrder", ""},
}

type esim struct{}

func (esim) Domain() string          { return "esim" }
func (esim) Supports(op string) bool { return supports(esimRoutes, op) }
func (esim) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(esimRoutes, t, map[string]string{"X-Admin-Protocol": "gsma/rsp/v2.5.0"})
}

// Interpret maps ES2+ functionExecutionStatus (always HTTP 200) to an outcome.
func (esim) Interpret(_ int, b map[string]any) (contract.Outcome, string, string) {
	h, _ := b["header"].(map[string]any)
	fes, _ := h["functionExecutionStatus"].(map[string]any)
	switch fes["status"] {
	case "Executed-Success", "Executed-WithWarning":
		return contract.Succeeded, "", ""
	case nil:
		return contract.Failed, "DXPS-3ES2:missing-status", "ES2+ response without functionExecutionStatus"
	}
	scd, _ := fes["statusCodeData"].(map[string]any)
	return contract.Failed, fmt.Sprintf("DXPS-3ES2:%v-%v", scd["subjectCode"], scd["reasonCode"]), fmt.Sprint(scd["message"])
}

// ---------------------------------------------------------------- bss: TMF666 account, TMF637 product, NPDB

var bssRoutes = map[string]route{
	"bss.account.create": {http.MethodPost, "/tmf-api/accountManagement/v5/billingAccount", ""},
	"bss.account.delete": {http.MethodDelete, "/tmf-api/accountManagement/v5/billingAccount/acc-{msisdn}", ""},
	"bss.bundle.create":  {http.MethodPost, "/tmf-api/productInventory/v5/product", ""},
	"npdb.port.put":      {http.MethodPut, "/npdb/v1/numbers/{msisdn}", ""},
	"npdb.port.delete":   {http.MethodDelete, "/npdb/v1/numbers/{msisdn}", ""},
}

type bss struct{}

func (bss) Domain() string                                                       { return "bss" }
func (bss) Supports(op string) bool                                              { return supports(bssRoutes, op) }
func (bss) Interpret(s int, b map[string]any) (contract.Outcome, string, string) { return ok2xx(s, b) }
func (bss) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(bssRoutes, t, map[string]string{"X-Request-ID": t.IdempotencyKey})
}

// ---------------------------------------------------------------- exposure: CAMARA QoD via NEF

var exposureRoutes = map[string]route{
	"camara.qod.create": {http.MethodPost, "/quality-on-demand/v1/sessions", ""},
	"camara.qod.delete": {http.MethodDelete, "/quality-on-demand/v1/sessions/msisdn/{msisdn}", ""},
}

type exposure struct{}

func (exposure) Domain() string          { return "exposure" }
func (exposure) Supports(op string) bool { return supports(exposureRoutes, op) }
func (exposure) Interpret(s int, b map[string]any) (contract.Outcome, string, string) {
	return ok2xx(s, b)
}
func (exposure) Build(_ context.Context, _ *Adapter, _ *registry.NE, t *contract.NeTask) (*Call, error) {
	return buildRoute(exposureRoutes, t, nil)
}
