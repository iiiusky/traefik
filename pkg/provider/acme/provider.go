package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	fmtlog "log"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/containous/traefik/pkg/config"
	"github.com/containous/traefik/pkg/log"
	"github.com/containous/traefik/pkg/rules"
	"github.com/containous/traefik/pkg/safe"
	traefiktls "github.com/containous/traefik/pkg/tls"
	"github.com/containous/traefik/pkg/types"
	"github.com/containous/traefik/pkg/version"
	"github.com/go-acme/lego/certificate"
	"github.com/go-acme/lego/challenge"
	"github.com/go-acme/lego/challenge/dns01"
	"github.com/go-acme/lego/lego"
	legolog "github.com/go-acme/lego/log"
	"github.com/go-acme/lego/providers/dns"
	"github.com/go-acme/lego/registration"
	"github.com/sirupsen/logrus"
)

var (
	// oscpMustStaple enables OSCP stapling as from https://github.com/go-acme/lego/issues/270
	oscpMustStaple = false
)

// Configuration holds ACME configuration provided by users
type Configuration struct {
	Email         string         `description:"Email address used for registration."`
	ACMELogging   bool           `description:"Enable debug logging of ACME actions."`
	CAServer      string         `description:"CA server to use."`
	Storage       string         `description:"Storage to use."`
	EntryPoint    string         `description:"EntryPoint to use."`
	KeyType       string         `description:"KeyType used for generating certificate private key. Allow value 'EC256', 'EC384', 'RSA2048', 'RSA4096', 'RSA8192'."`
	OnHostRule    bool           `description:"Enable certificate generation on router Host rules."`
	DNSChallenge  *DNSChallenge  `description:"Activate DNS-01 Challenge." label:"allowEmpty"`
	HTTPChallenge *HTTPChallenge `description:"Activate HTTP-01 Challenge." label:"allowEmpty"`
	TLSChallenge  *TLSChallenge  `description:"Activate TLS-ALPN-01 Challenge." label:"allowEmpty"`
	Domains       []types.Domain `description:"The list of domains for which certificates are generated on startup. Wildcard domains only accepted with DNSChallenge."`
}

// SetDefaults sets the default values.
func (a *Configuration) SetDefaults() {
	a.CAServer = lego.LEDirectoryProduction
	a.Storage = "acme.json"
	a.KeyType = "RSA4096"
}

// Certificate is a struct which contains all data needed from an ACME certificate
type Certificate struct {
	Domain      types.Domain
	Certificate []byte
	Key         []byte
}

// DNSChallenge contains DNS challenge Configuration
type DNSChallenge struct {
	Provider                string         `description:"Use a DNS-01 based challenge provider rather than HTTPS."`
	DelayBeforeCheck        types.Duration `description:"Assume DNS propagates after a delay in seconds rather than finding and querying nameservers."`
	Resolvers               []string       `description:"Use following DNS servers to resolve the FQDN authority."`
	DisablePropagationCheck bool           `description:"Disable the DNS propagation checks before notifying ACME that the DNS challenge is ready. [not recommended]"`
}

// HTTPChallenge contains HTTP challenge Configuration
type HTTPChallenge struct {
	EntryPoint string `description:"HTTP challenge EntryPoint"`
}

// TLSChallenge contains TLS challenge Configuration
type TLSChallenge struct{}

// Provider holds configurations of the provider.
type Provider struct {
	*Configuration
	Store                  Store
	certificates           []*Certificate
	account                *Account
	client                 *lego.Client
	certsChan              chan *Certificate
	configurationChan      chan<- config.Message
	tlsManager             *traefiktls.Manager
	clientMutex            sync.Mutex
	configFromListenerChan chan config.Configuration
	pool                   *safe.Pool
	resolvingDomains       map[string]struct{}
	resolvingDomainsMutex  sync.RWMutex
}

// SetTLSManager sets the tls manager to use
func (p *Provider) SetTLSManager(tlsManager *traefiktls.Manager) {
	p.tlsManager = tlsManager
}

// SetConfigListenerChan initializes the configFromListenerChan
func (p *Provider) SetConfigListenerChan(configFromListenerChan chan config.Configuration) {
	p.configFromListenerChan = configFromListenerChan
}

// ListenConfiguration sets a new Configuration into the configFromListenerChan
func (p *Provider) ListenConfiguration(config config.Configuration) {
	p.configFromListenerChan <- config
}

// ListenRequest resolves new certificates for a domain from an incoming request and return a valid Certificate to serve (onDemand option)
func (p *Provider) ListenRequest(domain string) (*tls.Certificate, error) {
	ctx := log.With(context.Background(), log.Str(log.ProviderName, "acme"))

	acmeCert, err := p.resolveCertificate(ctx, types.Domain{Main: domain}, false)
	if acmeCert == nil || err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(acmeCert.Certificate, acmeCert.PrivateKey)

	return &cert, err
}

// Init for compatibility reason the BaseProvider implements an empty Init
func (p *Provider) Init() error {

	ctx := log.With(context.Background(), log.Str(log.ProviderName, "acme"))
	logger := log.FromContext(ctx)

	if p.ACMELogging {
		legolog.Logger = fmtlog.New(logger.WriterLevel(logrus.InfoLevel), "legolog: ", 0)
	} else {
		legolog.Logger = fmtlog.New(ioutil.Discard, "", 0)
	}

	if len(p.Configuration.Storage) == 0 {
		return errors.New("unable to initialize ACME provider with no storage location for the certificates")
	}
	p.Store = NewLocalStore(p.Configuration.Storage)

	var err error
	p.account, err = p.Store.GetAccount()
	if err != nil {
		return fmt.Errorf("unable to get ACME account : %v", err)
	}

	// Reset Account if caServer changed, thus registration URI can be updated
	if p.account != nil && p.account.Registration != nil && !isAccountMatchingCaServer(ctx, p.account.Registration.URI, p.CAServer) {
		logger.Info("Account URI does not match the current CAServer. The account will be reset.")
		p.account = nil
	}

	p.certificates, err = p.Store.GetCertificates()
	if err != nil {
		return fmt.Errorf("unable to get ACME certificates : %v", err)
	}

	// Init the currently resolved domain map
	p.resolvingDomains = make(map[string]struct{})

	return nil
}

func isAccountMatchingCaServer(ctx context.Context, accountURI string, serverURI string) bool {
	logger := log.FromContext(ctx)

	aru, err := url.Parse(accountURI)
	if err != nil {
		logger.Infof("Unable to parse account.Registration URL: %v", err)
		return false
	}

	cau, err := url.Parse(serverURI)
	if err != nil {
		logger.Infof("Unable to parse CAServer URL: %v", err)
		return false
	}

	return cau.Hostname() == aru.Hostname()
}

// Provide allows the file provider to provide configurations to traefik
// using the given Configuration channel.
func (p *Provider) Provide(configurationChan chan<- config.Message, pool *safe.Pool) error {
	ctx := log.With(context.Background(), log.Str(log.ProviderName, "acme"))

	p.pool = pool

	p.watchCertificate(ctx)
	p.watchNewDomains(ctx)

	p.configurationChan = configurationChan
	p.refreshCertificates()

	p.deleteUnnecessaryDomains(ctx)
	for i := 0; i < len(p.Domains); i++ {
		domain := p.Domains[i]
		safe.Go(func() {
			if _, err := p.resolveCertificate(ctx, domain, true); err != nil {
				log.WithoutContext().WithField(log.ProviderName, "acme").
					Errorf("Unable to obtain ACME certificate for domains %q : %v", strings.Join(domain.ToStrArray(), ","), err)
			}
		})
	}

	p.renewCertificates(ctx)

	ticker := time.NewTicker(24 * time.Hour)
	pool.Go(func(stop chan bool) {
		for {
			select {
			case <-ticker.C:
				p.renewCertificates(ctx)
			case <-stop:
				ticker.Stop()
				return
			}
		}
	})

	return nil
}

func (p *Provider) getClient() (*lego.Client, error) {
	p.clientMutex.Lock()
	defer p.clientMutex.Unlock()

	ctx := log.With(context.Background(), log.Str(log.ProviderName, "acme"))
	logger := log.FromContext(ctx)

	if p.client != nil {
		return p.client, nil
	}

	account, err := p.initAccount(ctx)
	if err != nil {
		return nil, err
	}

	logger.Debug("Building ACME client...")

	caServer := lego.LEDirectoryProduction
	if len(p.CAServer) > 0 {
		caServer = p.CAServer
	}
	logger.Debug(caServer)

	config := lego.NewConfig(account)
	config.CADirURL = caServer
	config.Certificate.KeyType = account.KeyType
	config.UserAgent = fmt.Sprintf("containous-traefik/%s", version.Version)

	client, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}

	// New users will need to register; be sure to save it
	if account.GetRegistration() == nil {
		logger.Info("Register...")

		reg, errR := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if errR != nil {
			return nil, errR
		}

		account.Registration = reg
	}

	// Save the account once before all the certificates generation/storing
	// No certificate can be generated if account is not initialized
	err = p.Store.SaveAccount(account)
	if err != nil {
		return nil, err
	}

	switch {
	case p.DNSChallenge != nil && len(p.DNSChallenge.Provider) > 0:
		logger.Debugf("Using DNS Challenge provider: %s", p.DNSChallenge.Provider)

		var provider challenge.Provider
		provider, err = dns.NewDNSChallengeProviderByName(p.DNSChallenge.Provider)
		if err != nil {
			return nil, err
		}

		err = client.Challenge.SetDNS01Provider(provider,
			dns01.CondOption(len(p.DNSChallenge.Resolvers) > 0, dns01.AddRecursiveNameservers(p.DNSChallenge.Resolvers)),
			dns01.CondOption(p.DNSChallenge.DisablePropagationCheck || p.DNSChallenge.DelayBeforeCheck > 0,
				dns01.AddPreCheck(func(_, _ string) (bool, error) {
					if p.DNSChallenge.DelayBeforeCheck > 0 {
						log.Debugf("Delaying %d rather than validating DNS propagation now.", p.DNSChallenge.DelayBeforeCheck)
						time.Sleep(time.Duration(p.DNSChallenge.DelayBeforeCheck))
					}
					return true, nil
				})),
		)
		if err != nil {
			return nil, err
		}

	case p.HTTPChallenge != nil && len(p.HTTPChallenge.EntryPoint) > 0:
		logger.Debug("Using HTTP Challenge provider.")

		err = client.Challenge.SetHTTP01Provider(&challengeHTTP{Store: p.Store})
		if err != nil {
			return nil, err
		}

	case p.TLSChallenge != nil:
		logger.Debug("Using TLS Challenge provider.")

		err = client.Challenge.SetTLSALPN01Provider(&challengeTLSALPN{Store: p.Store})
		if err != nil {
			return nil, err
		}

	default:
		return nil, errors.New("ACME challenge not specified, please select TLS or HTTP or DNS Challenge")
	}

	p.client = client
	return p.client, nil
}

func (p *Provider) initAccount(ctx context.Context) (*Account, error) {
	if p.account == nil || len(p.account.Email) == 0 {
		var err error
		p.account, err = NewAccount(ctx, p.Email, p.KeyType)
		if err != nil {
			return nil, err
		}
	}

	// Set the KeyType if not already defined in the account
	if len(p.account.KeyType) == 0 {
		p.account.KeyType = GetKeyType(ctx, p.KeyType)
	}

	return p.account, nil
}

func (p *Provider) resolveDomains(ctx context.Context, domains []string) {
	if len(domains) == 0 {
		log.FromContext(ctx).Debug("No domain parsed in provider ACME")
		return
	}

	log.FromContext(ctx).Debugf("Try to challenge certificate for domain %v founded in HostSNI rule", domains)

	var domain types.Domain
	if len(domains) > 0 {
		domain = types.Domain{Main: domains[0]}
		if len(domains) > 1 {
			domain.SANs = domains[1:]
		}

		safe.Go(func() {
			if _, err := p.resolveCertificate(ctx, domain, false); err != nil {
				log.FromContext(ctx).Errorf("Unable to obtain ACME certificate for domains %q: %v", strings.Join(domains, ","), err)
			}
		})
	}
}

func (p *Provider) watchNewDomains(ctx context.Context) {
	p.pool.Go(func(stop chan bool) {
		for {
			select {
			case config := <-p.configFromListenerChan:
				if config.TCP != nil {
					for routerName, route := range config.TCP.Routers {
						if route.TLS == nil {
							continue
						}
						ctxRouter := log.With(ctx, log.Str(log.RouterName, routerName), log.Str(log.Rule, route.Rule))

						domains, err := rules.ParseHostSNI(route.Rule)
						if err != nil {
							log.FromContext(ctxRouter).Errorf("Error parsing domains in provider ACME: %v", err)
							continue
						}
						p.resolveDomains(ctxRouter, domains)
					}
				}

				for routerName, route := range config.HTTP.Routers {
					if route.TLS == nil {
						continue
					}
					ctxRouter := log.With(ctx, log.Str(log.RouterName, routerName), log.Str(log.Rule, route.Rule))

					domains, err := rules.ParseDomains(route.Rule)
					if err != nil {
						log.FromContext(ctxRouter).Errorf("Error parsing domains in provider ACME: %v", err)
						continue
					}
					p.resolveDomains(ctxRouter, domains)
				}
			case <-stop:
				return
			}
		}
	})
}

func (p *Provider) resolveCertificate(ctx context.Context, domain types.Domain, domainFromConfigurationFile bool) (*certificate.Resource, error) {
	domains, err := p.getValidDomains(ctx, domain, domainFromConfigurationFile)
	if err != nil {
		return nil, err
	}

	// Check provided certificates
	uncheckedDomains := p.getUncheckedDomains(ctx, domains, !domainFromConfigurationFile)
	if len(uncheckedDomains) == 0 {
		return nil, nil
	}

	p.addResolvingDomains(uncheckedDomains)
	defer p.removeResolvingDomains(uncheckedDomains)

	logger := log.FromContext(ctx)
	logger.Debugf("Loading ACME certificates %+v...", uncheckedDomains)

	client, err := p.getClient()
	if err != nil {
		return nil, fmt.Errorf("cannot get ACME client %v", err)
	}

	request := certificate.ObtainRequest{
		Domains:    domains,
		Bundle:     true,
		MustStaple: oscpMustStaple,
	}

	cert, err := client.Certificate.Obtain(request)
	if err != nil {
		return nil, fmt.Errorf("unable to generate a certificate for the domains %v: %v", uncheckedDomains, err)
	}
	if cert == nil {
		return nil, fmt.Errorf("domains %v do not generate a certificate", uncheckedDomains)
	}
	if len(cert.Certificate) == 0 || len(cert.PrivateKey) == 0 {
		return nil, fmt.Errorf("domains %v generate certificate with no value: %v", uncheckedDomains, cert)
	}

	logger.Debugf("Certificates obtained for domains %+v", uncheckedDomains)

	if len(uncheckedDomains) > 1 {
		domain = types.Domain{Main: uncheckedDomains[0], SANs: uncheckedDomains[1:]}
	} else {
		domain = types.Domain{Main: uncheckedDomains[0]}
	}
	p.addCertificateForDomain(domain, cert.Certificate, cert.PrivateKey)

	return cert, nil
}

func (p *Provider) removeResolvingDomains(resolvingDomains []string) {
	p.resolvingDomainsMutex.Lock()
	defer p.resolvingDomainsMutex.Unlock()

	for _, domain := range resolvingDomains {
		delete(p.resolvingDomains, domain)
	}
}

func (p *Provider) addResolvingDomains(resolvingDomains []string) {
	p.resolvingDomainsMutex.Lock()
	defer p.resolvingDomainsMutex.Unlock()

	for _, domain := range resolvingDomains {
		p.resolvingDomains[domain] = struct{}{}
	}
}

func (p *Provider) addCertificateForDomain(domain types.Domain, certificate []byte, key []byte) {
	p.certsChan <- &Certificate{Certificate: certificate, Key: key, Domain: domain}
}

// deleteUnnecessaryDomains deletes from the configuration :
// - Duplicated domains
// - Domains which are checked by wildcard domain
func (p *Provider) deleteUnnecessaryDomains(ctx context.Context) {
	var newDomains []types.Domain

	logger := log.FromContext(ctx)

	for idxDomainToCheck, domainToCheck := range p.Domains {
		keepDomain := true

		for idxDomain, domain := range p.Domains {
			if idxDomainToCheck == idxDomain {
				continue
			}

			if reflect.DeepEqual(domain, domainToCheck) {
				if idxDomainToCheck > idxDomain {
					logger.Warnf("The domain %v is duplicated in the configuration but will be process by ACME provider only once.", domainToCheck)
					keepDomain = false
				}
				break
			}

			// Check if CN or SANS to check already exists
			// or can not be checked by a wildcard
			var newDomainsToCheck []string
			for _, domainProcessed := range domainToCheck.ToStrArray() {
				if idxDomain < idxDomainToCheck && isDomainAlreadyChecked(domainProcessed, domain.ToStrArray()) {
					// The domain is duplicated in a CN
					logger.Warnf("Domain %q is duplicated in the configuration or validated by the domain %v. It will be processed once.", domainProcessed, domain)
					continue
				} else if domain.Main != domainProcessed && strings.HasPrefix(domain.Main, "*") && isDomainAlreadyChecked(domainProcessed, []string{domain.Main}) {
					// Check if a wildcard can validate the domain
					logger.Warnf("Domain %q will not be processed by ACME provider because it is validated by the wildcard %q", domainProcessed, domain.Main)
					continue
				}
				newDomainsToCheck = append(newDomainsToCheck, domainProcessed)
			}

			// Delete the domain if both Main and SANs can be validated by the wildcard domain
			// otherwise keep the unchecked values
			if newDomainsToCheck == nil {
				keepDomain = false
				break
			}
			domainToCheck.Set(newDomainsToCheck)
		}

		if keepDomain {
			newDomains = append(newDomains, domainToCheck)
		}
	}

	p.Domains = newDomains
}

func (p *Provider) watchCertificate(ctx context.Context) {
	p.certsChan = make(chan *Certificate)

	p.pool.Go(func(stop chan bool) {
		for {
			select {
			case cert := <-p.certsChan:
				certUpdated := false
				for _, domainsCertificate := range p.certificates {
					if reflect.DeepEqual(cert.Domain, domainsCertificate.Domain) {
						domainsCertificate.Certificate = cert.Certificate
						domainsCertificate.Key = cert.Key
						certUpdated = true
						break
					}
				}
				if !certUpdated {
					p.certificates = append(p.certificates, cert)
				}

				err := p.saveCertificates()
				if err != nil {
					log.FromContext(ctx).Error(err)
				}
			case <-stop:
				return
			}
		}
	})
}

func (p *Provider) saveCertificates() error {
	err := p.Store.SaveCertificates(p.certificates)

	p.refreshCertificates()

	return err
}

func (p *Provider) refreshCertificates() {
	conf := config.Message{
		ProviderName: "ACME",
		Configuration: &config.Configuration{
			HTTP: &config.HTTPConfiguration{
				Routers:     map[string]*config.Router{},
				Middlewares: map[string]*config.Middleware{},
				Services:    map[string]*config.Service{},
			},
			TLS: []*traefiktls.Configuration{},
		},
	}

	for _, cert := range p.certificates {
		cert := &traefiktls.Certificate{CertFile: traefiktls.FileOrContent(cert.Certificate), KeyFile: traefiktls.FileOrContent(cert.Key)}
		conf.Configuration.TLS = append(conf.Configuration.TLS, &traefiktls.Configuration{Certificate: cert})
	}
	p.configurationChan <- conf
}

func (p *Provider) renewCertificates(ctx context.Context) {
	logger := log.FromContext(ctx)

	logger.Info("Testing certificate renew...")
	for _, cert := range p.certificates {
		crt, err := getX509Certificate(ctx, cert)
		// If there's an error, we assume the cert is broken, and needs update
		// <= 30 days left, renew certificate
		if err != nil || crt == nil || crt.NotAfter.Before(time.Now().Add(24*30*time.Hour)) {
			client, err := p.getClient()
			if err != nil {
				logger.Infof("Error renewing certificate from LE : %+v, %v", cert.Domain, err)
				continue
			}

			logger.Infof("Renewing certificate from LE : %+v", cert.Domain)

			renewedCert, err := client.Certificate.Renew(certificate.Resource{
				Domain:      cert.Domain.Main,
				PrivateKey:  cert.Key,
				Certificate: cert.Certificate,
			}, true, oscpMustStaple)

			if err != nil {
				logger.Errorf("Error renewing certificate from LE: %v, %v", cert.Domain, err)
				continue
			}

			if len(renewedCert.Certificate) == 0 || len(renewedCert.PrivateKey) == 0 {
				logger.Errorf("domains %v renew certificate with no value: %v", cert.Domain.ToStrArray(), cert)
				continue
			}

			p.addCertificateForDomain(cert.Domain, renewedCert.Certificate, renewedCert.PrivateKey)
		}
	}
}

// Get provided certificate which check a domains list (Main and SANs)
// from static and dynamic provided certificates
func (p *Provider) getUncheckedDomains(ctx context.Context, domainsToCheck []string, checkConfigurationDomains bool) []string {
	p.resolvingDomainsMutex.RLock()
	defer p.resolvingDomainsMutex.RUnlock()

	log.FromContext(ctx).Debugf("Looking for provided certificate(s) to validate %q...", domainsToCheck)

	allDomains := p.tlsManager.GetStore("default").GetAllDomains()

	// Get ACME certificates
	for _, cert := range p.certificates {
		allDomains = append(allDomains, strings.Join(cert.Domain.ToStrArray(), ","))
	}

	// Get currently resolved domains
	for domain := range p.resolvingDomains {
		allDomains = append(allDomains, domain)
	}

	// Get Configuration Domains
	if checkConfigurationDomains {
		for i := 0; i < len(p.Domains); i++ {
			allDomains = append(allDomains, strings.Join(p.Domains[i].ToStrArray(), ","))
		}
	}

	return searchUncheckedDomains(ctx, domainsToCheck, allDomains)
}

func searchUncheckedDomains(ctx context.Context, domainsToCheck []string, existentDomains []string) []string {
	var uncheckedDomains []string
	for _, domainToCheck := range domainsToCheck {
		if !isDomainAlreadyChecked(domainToCheck, existentDomains) {
			uncheckedDomains = append(uncheckedDomains, domainToCheck)
		}
	}

	logger := log.FromContext(ctx)
	if len(uncheckedDomains) == 0 {
		logger.Debugf("No ACME certificate generation required for domains %q.", domainsToCheck)
	} else {
		logger.Debugf("Domains %q need ACME certificates generation for domains %q.", domainsToCheck, strings.Join(uncheckedDomains, ","))
	}
	return uncheckedDomains
}

func getX509Certificate(ctx context.Context, cert *Certificate) (*x509.Certificate, error) {
	logger := log.FromContext(ctx)

	tlsCert, err := tls.X509KeyPair(cert.Certificate, cert.Key)
	if err != nil {
		logger.Errorf("Failed to load TLS key pair from ACME certificate for domain %q (SAN : %q), certificate will be renewed : %v", cert.Domain.Main, strings.Join(cert.Domain.SANs, ","), err)
		return nil, err
	}

	crt := tlsCert.Leaf
	if crt == nil {
		crt, err = x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			logger.Errorf("Failed to parse TLS key pair from ACME certificate for domain %q (SAN : %q), certificate will be renewed : %v", cert.Domain.Main, strings.Join(cert.Domain.SANs, ","), err)
		}
	}

	return crt, err
}

// getValidDomains checks if given domain is allowed to generate a ACME certificate and return it
func (p *Provider) getValidDomains(ctx context.Context, domain types.Domain, wildcardAllowed bool) ([]string, error) {
	domains := domain.ToStrArray()
	if len(domains) == 0 {
		return nil, errors.New("unable to generate a certificate in ACME provider when no domain is given")
	}

	if strings.HasPrefix(domain.Main, "*") {
		if !wildcardAllowed {
			return nil, fmt.Errorf("unable to generate a wildcard certificate in ACME provider for domain %q from a 'Host' rule", strings.Join(domains, ","))
		}

		if p.DNSChallenge == nil {
			return nil, fmt.Errorf("unable to generate a wildcard certificate in ACME provider for domain %q : ACME needs a DNSChallenge", strings.Join(domains, ","))
		}

		if strings.HasPrefix(domain.Main, "*.*") {
			return nil, fmt.Errorf("unable to generate a wildcard certificate in ACME provider for domain %q : ACME does not allow '*.*' wildcard domain", strings.Join(domains, ","))
		}
	}

	var cleanDomains []string
	for _, domain := range domains {
		canonicalDomain := types.CanonicalDomain(domain)
		cleanDomain := dns01.UnFqdn(canonicalDomain)
		if canonicalDomain != cleanDomain {
			log.FromContext(ctx).Warnf("FQDN detected, please remove the trailing dot: %s", canonicalDomain)
		}
		cleanDomains = append(cleanDomains, cleanDomain)
	}

	return cleanDomains, nil
}

func isDomainAlreadyChecked(domainToCheck string, existentDomains []string) bool {
	for _, certDomains := range existentDomains {
		for _, certDomain := range strings.Split(certDomains, ",") {
			if types.MatchDomain(domainToCheck, certDomain) {
				return true
			}
		}
	}
	return false
}
