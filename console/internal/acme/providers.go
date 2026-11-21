package acme

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-acme/lego/v5/challenge"
	"github.com/go-acme/lego/v5/providers/dns/acmedns"
	"github.com/go-acme/lego/v5/providers/dns/azuredns"
	"github.com/go-acme/lego/v5/providers/dns/cloudflare"
	"github.com/go-acme/lego/v5/providers/dns/desec"
	"github.com/go-acme/lego/v5/providers/dns/digitalocean"
	"github.com/go-acme/lego/v5/providers/dns/dnsupdate"
	"github.com/go-acme/lego/v5/providers/dns/gandiv5"
	"github.com/go-acme/lego/v5/providers/dns/gcloud"
	"github.com/go-acme/lego/v5/providers/dns/hetzner"
	"github.com/go-acme/lego/v5/providers/dns/ovh"
	"github.com/go-acme/lego/v5/providers/dns/porkbun"
	"github.com/go-acme/lego/v5/providers/dns/route53"
	"github.com/nrdcg/goacmedns"

	"patai/console/internal/durable"
)

// The DNS providers the console offers (patai_docs notes/remote-access.md, decision 3, agreed 2026-11-21): a short
// list rather than all of lego's, for size. Each asks for what its API needs, and nothing else.

// Field is one thing the owner gives for a provider.
type Field struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Hint      string `json:"hint,omitempty"`
	Secret    bool   `json:"secret,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	Multiline bool   `json:"multiline,omitempty"`
}

// Provider is a DNS provider, with what the owner gives for it.
type Provider struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Fields []Field `json:"fields"`
	// Narrow says how to make the narrowest credential the provider has, in a line.
	Narrow string `json:"narrow,omitempty"`

	build func(values map[string]string, dir string) (challenge.Provider, error)
}

var token = Field{Name: "token", Label: "API token", Secret: true}

// Providers is the list, in the order the page shows it.
var Providers = []Provider{
	{ID: "cloudflare", Title: "Cloudflare", Fields: []Field{token},
		Narrow: "Make a token with Zone, DNS, Edit for this zone only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := cloudflare.NewDefaultConfig()
			c.AuthToken = v["token"]
			return cloudflare.NewDNSProviderConfig(c)
		}},
	{ID: "route53", Title: "Amazon Route 53", Fields: []Field{
		{Name: "access_key_id", Label: "Access key ID"},
		{Name: "secret_access_key", Label: "Secret access key", Secret: true},
		{Name: "hosted_zone_id", Label: "Hosted zone ID", Optional: true},
		{Name: "region", Label: "Region", Optional: true, Hint: "us-east-1 if empty"},
	}, Narrow: "Use an IAM user whose policy allows route53:ChangeResourceRecordSets on this hosted zone only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := route53.NewDefaultConfig()
			c.AccessKeyID, c.SecretAccessKey, c.HostedZoneID = v["access_key_id"], v["secret_access_key"], v["hosted_zone_id"]
			c.Region = v["region"]
			if c.Region == "" {
				c.Region = "us-east-1"
			}
			return route53.NewDNSProviderConfig(c)
		}},
	{ID: "gcloud", Title: "Google Cloud DNS", Fields: []Field{
		{Name: "service_account", Label: "Service account key", Secret: true, Multiline: true, Hint: "The key's JSON"},
	}, Narrow: "Use a service account with DNS Administrator on this project only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			return gcloud.NewDNSProviderServiceAccountKey([]byte(v["service_account"]))
		}},
	{ID: "azuredns", Title: "Azure DNS", Fields: []Field{
		{Name: "tenant_id", Label: "Tenant ID"},
		{Name: "client_id", Label: "Client ID"},
		{Name: "client_secret", Label: "Client secret", Secret: true},
		{Name: "subscription_id", Label: "Subscription ID"},
		{Name: "resource_group", Label: "Resource group"},
	}, Narrow: "Give the app registration DNS Zone Contributor on this zone only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := azuredns.NewDefaultConfig()
			c.TenantID, c.ClientID, c.ClientSecret = v["tenant_id"], v["client_id"], v["client_secret"]
			c.SubscriptionID, c.ResourceGroup, c.AuthMethod = v["subscription_id"], v["resource_group"], "env"
			return azuredns.NewDNSProviderConfig(c)
		}},
	{ID: "digitalocean", Title: "DigitalOcean", Fields: []Field{token},
		Narrow: "Make a token with the domain scopes only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := digitalocean.NewDefaultConfig()
			c.AuthToken = v["token"]
			return digitalocean.NewDNSProviderConfig(c)
		}},
	{ID: "hetzner", Title: "Hetzner", Fields: []Field{token},
		Narrow: "Make the token in the project that holds this zone.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := hetzner.NewDefaultConfig()
			c.APIToken = v["token"]
			return hetzner.NewDNSProviderConfig(c)
		}},
	{ID: "ovh", Title: "OVHcloud", Fields: []Field{
		{Name: "endpoint", Label: "Endpoint", Hint: "ovh-ca, ovh-eu or ovh-us"},
		{Name: "application_key", Label: "Application key"},
		{Name: "application_secret", Label: "Application secret", Secret: true},
		{Name: "consumer_key", Label: "Consumer key", Secret: true},
	}, Narrow: "Give the consumer key GET, POST and DELETE on /domain/zone/<zone>/* only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := ovh.NewDefaultConfig()
			c.APIEndpoint, c.ApplicationKey = v["endpoint"], v["application_key"]
			c.ApplicationSecret, c.ConsumerKey = v["application_secret"], v["consumer_key"]
			return ovh.NewDNSProviderConfig(c)
		}},
	{ID: "gandiv5", Title: "Gandi", Fields: []Field{{Name: "token", Label: "Personal access token", Secret: true}},
		Narrow: "Give the token Manage domain name technical configurations, for this domain only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := gandiv5.NewDefaultConfig()
			c.PersonalAccessToken = v["token"]
			return gandiv5.NewDNSProviderConfig(c)
		}},
	{ID: "porkbun", Title: "Porkbun", Fields: []Field{
		{Name: "api_key", Label: "API key"},
		{Name: "secret_api_key", Label: "Secret API key", Secret: true},
	}, Narrow: "Turn on API access for this domain only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := porkbun.NewDefaultConfig()
			c.APIKey, c.SecretAPIKey = v["api_key"], v["secret_api_key"]
			return porkbun.NewDNSProviderConfig(c)
		}},
	{ID: "desec", Title: "deSEC", Fields: []Field{token},
		Narrow: "Make a token limited to this domain's _acme-challenge TXT record.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := desec.NewDefaultConfig()
			c.Token = v["token"]
			return desec.NewDNSProviderConfig(c)
		}},
	{ID: "dnsupdate", Title: "RFC 2136 (DNS UPDATE)", Fields: []Field{
		{Name: "nameserver", Label: "Name server", Hint: "host:port of the primary"},
		{Name: "tsig_key", Label: "TSIG key name"},
		{Name: "tsig_algorithm", Label: "TSIG algorithm", Optional: true, Hint: "hmac-sha256. if empty"},
		{Name: "tsig_secret", Label: "TSIG secret", Secret: true},
	}, Narrow: "Give the key update rights for this name's _acme-challenge TXT only.",
		build: func(v map[string]string, _ string) (challenge.Provider, error) {
			c := dnsupdate.NewDefaultConfig()
			c.Nameserver, c.TSIGKey, c.TSIGSecret = v["nameserver"], v["tsig_key"], v["tsig_secret"]
			c.TSIGAlgorithm = v["tsig_algorithm"]
			if c.TSIGAlgorithm == "" {
				c.TSIGAlgorithm = "hmac-sha256."
			}
			return dnsupdate.NewDNSProviderConfig(c)
		}},
	{ID: "acmedns", Title: "acme-dns", Fields: []Field{
		{Name: "server", Label: "Server", Hint: "https://auth.example.com"},
		{Name: "username", Label: "Username"},
		{Name: "password", Label: "Password", Secret: true},
		{Name: "subdomain", Label: "Subdomain"},
		{Name: "fulldomain", Label: "Full domain", Hint: "What _acme-challenge.<name> is a CNAME of"},
	}, Narrow: "acme-dns accounts can only ever write their own TXT record.",
		build: buildACMEDNS},
}

// buildACMEDNS writes the owner's acme-dns account where lego looks for it, in the console's own directory, as lego's
// provider keeps accounts in a file of its own.
func buildACMEDNS(v map[string]string, dir string) (challenge.Provider, error) {
	if dir == "" {
		return nil, errors.New("acme-dns needs somewhere to keep its account")
	}
	return &acmeDNS{values: v, path: filepath.Join(dir, "domain-acmedns.json")}, nil
}

// acmeDNS makes lego's provider for the name it's asked about, with the owner's account for that name.
type acmeDNS struct {
	values map[string]string
	path   string
}

func (a *acmeDNS) provider(domain string) (challenge.Provider, error) {
	accounts := map[string]goacmedns.Account{strings.TrimSuffix(domain, "."): {
		FullDomain: a.values["fulldomain"], SubDomain: a.values["subdomain"], Username: a.values["username"],
		Password: a.values["password"], ServerURL: a.values["server"],
	}}
	data, err := json.Marshal(accounts)
	if err != nil {
		return nil, err
	}
	if err := durable.Write(a.path, data, 0o600); err != nil {
		return nil, err
	}
	c := acmedns.NewDefaultConfig()
	c.APIBase, c.StoragePath = a.values["server"], a.path
	return acmedns.NewDNSProviderConfig(c)
}

func (a *acmeDNS) Present(ctx context.Context, domain, token, keyAuth string) error {
	p, err := a.provider(domain)
	if err != nil {
		return err
	}
	return p.Present(ctx, domain, token, keyAuth)
}

func (a *acmeDNS) CleanUp(ctx context.Context, domain, token, keyAuth string) error {
	p, err := a.provider(domain)
	if err != nil {
		return err
	}
	return p.CleanUp(ctx, domain, token, keyAuth)
}

// Build makes the provider id with the owner's values, after checking the fields it needs are there. dir is where a
// provider that keeps a file of its own keeps it.
func Build(id string, values map[string]string, dir string) (challenge.Provider, error) {
	p := Find(id)
	if p == nil {
		return nil, fmt.Errorf("%q isn't a provider here", id)
	}
	clean := map[string]string{}
	for _, f := range p.Fields {
		value := strings.TrimSpace(values[f.Name])
		if value == "" && !f.Optional {
			return nil, fmt.Errorf("%s is missing", f.Label)
		}
		if len(value) > 8192 {
			return nil, fmt.Errorf("%s is too long", f.Label)
		}
		clean[f.Name] = value
	}
	return p.build(clean, dir)
}

// Find is the provider with id, or nil.
func Find(id string) *Provider {
	for i := range Providers {
		if Providers[i].ID == id {
			return &Providers[i]
		}
	}
	return nil
}

// Keep is the owner's values for the provider's fields, and nothing else, for keeping.
func Keep(id string, values map[string]string) map[string]string {
	p := Find(id)
	out := map[string]string{}
	if p == nil {
		return out
	}
	for _, f := range p.Fields {
		if value := strings.TrimSpace(values[f.Name]); value != "" {
			out[f.Name] = value
		}
	}
	return out
}
