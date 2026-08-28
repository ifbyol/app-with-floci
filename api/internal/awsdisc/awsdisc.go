// Package awsdisc discovers the app's backing endpoints by calling Floci's AWS
// control plane, the same way an application would call real AWS.
//
// Two of the three services can be used exactly as advertised. OpenSearch
// cannot, and the difference is deliberately surfaced rather than hidden - see
// the Note field on ServiceState.
package awsdisc

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/okteto/app-with-floci/api/internal/config"
)

type Endpoint struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}

func (e Endpoint) Addr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port)))
}

func (e Endpoint) OK() bool { return e.Host != "" && e.Port > 0 }

// ServiceState is one row of the emulator inspector: which control-plane call
// was made, what it reported, and what the app ended up using.
type ServiceState struct {
	Service    string `json:"service"`
	API        string `json:"api"`
	Resource   string `json:"resource"`
	Status     string `json:"status"`
	Ready      bool   `json:"ready"`
	Advertised string `json:"advertised"`
	Effective  string `json:"effective"`
	Note       string `json:"note,omitempty"`
}

type Snapshot struct {
	Ready    bool           `json:"ready"`
	Services []ServiceState `json:"services"`

	DB        Endpoint `json:"-"`
	Cache     Endpoint `json:"-"`
	SearchURL string   `json:"-"`
}

type Discoverer struct {
	// resolvable caches DNS answers for advertised hostnames.
	resolvable sync.Map

	rdsc *rds.Client
	ecc  *elasticache.Client
	osc  *opensearch.Client
	cfg  *config.Config
}

// New builds the three control-plane clients.
//
// The aws.Config is constructed literally rather than through
// LoadDefaultConfig: that keeps the SDK from reading ~/.aws or AWS_PROFILE, so
// this process can only ever talk to the endpoint it was given. Credentials are
// placeholders - Floci validates the SigV4 shape, not the secret.
func New(cfg *config.Config) *Discoverer {
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider("floci", "floci", ""),
	}
	base := aws.String(cfg.FlociEndpoint)

	return &Discoverer{
		cfg:  cfg,
		rdsc: rds.NewFromConfig(awsCfg, func(o *rds.Options) { o.BaseEndpoint = base }),
		ecc:  elasticache.NewFromConfig(awsCfg, func(o *elasticache.Options) { o.BaseEndpoint = base }),
		osc:  opensearch.NewFromConfig(awsCfg, func(o *opensearch.Options) { o.BaseEndpoint = base }),
	}
}

// Probe performs one discovery pass without waiting.
func (d *Discoverer) Probe(ctx context.Context) *Snapshot {
	snap := &Snapshot{}
	db := d.probeRDS(ctx, snap)
	cache := d.probeElastiCache(ctx, snap)
	search := d.probeOpenSearch(ctx, snap)

	snap.DB, snap.Cache, snap.SearchURL = db, cache, search
	snap.Ready = db.OK() && cache.OK() && search != ""
	return snap
}

func (d *Discoverer) probeRDS(ctx context.Context, snap *Snapshot) Endpoint {
	st := ServiceState{
		Service: "RDS", API: "DescribeDBInstances", Resource: d.cfg.DBInstanceID,
		Note: "Floci proxies the PostgreSQL wire protocol; the engine container publishes no ports.",
	}
	defer func() { snap.Services = append(snap.Services, st) }()

	out, err := d.rdsc.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(d.cfg.DBInstanceID),
	})
	if err != nil || len(out.DBInstances) == 0 {
		st.Status = describeErr(err, "instance not found")
		return Endpoint{}
	}

	inst := out.DBInstances[0]
	st.Status = aws.ToString(inst.DBInstanceStatus)
	if inst.Endpoint == nil || aws.ToString(inst.Endpoint.Address) == "" {
		return Endpoint{}
	}

	ep := Endpoint{
		Host: aws.ToString(inst.Endpoint.Address),
		Port: aws.ToInt32(inst.Endpoint.Port),
	}
	st.Advertised = ep.Addr()
	st.Effective = ep.Addr()
	st.Ready = st.Status == "available" && ep.OK()
	return ep
}

func (d *Discoverer) probeElastiCache(ctx context.Context, snap *Snapshot) Endpoint {
	st := ServiceState{
		Service: "ElastiCache", API: "DescribeReplicationGroups", Resource: d.cfg.CacheGroupID,
		Note: "Read from ConfigurationEndpoint: Floci leaves NodeGroups[0].PrimaryEndpoint null.",
	}
	defer func() { snap.Services = append(snap.Services, st) }()

	out, err := d.ecc.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{
		ReplicationGroupId: aws.String(d.cfg.CacheGroupID),
	})
	if err != nil || len(out.ReplicationGroups) == 0 {
		st.Status = describeErr(err, "replication group not found")
		return Endpoint{}
	}

	g := out.ReplicationGroups[0]
	st.Status = aws.ToString(g.Status)

	cep := g.ConfigurationEndpoint
	if cep == nil && len(g.NodeGroups) > 0 {
		cep = g.NodeGroups[0].PrimaryEndpoint
	}
	if cep == nil || aws.ToString(cep.Address) == "" {
		return Endpoint{}
	}

	ep := Endpoint{Host: aws.ToString(cep.Address), Port: aws.ToInt32(cep.Port)}
	st.Advertised = ep.Addr()
	st.Effective = ep.Addr()
	st.Ready = st.Status == "available" && ep.OK()
	return ep
}

func (d *Discoverer) probeOpenSearch(ctx context.Context, snap *Snapshot) string {
	st := ServiceState{
		Service: "OpenSearch", API: "DescribeDomain", Resource: d.cfg.SearchDomain,
	}
	defer func() { snap.Services = append(snap.Services, st) }()

	out, err := d.osc.DescribeDomain(ctx, &opensearch.DescribeDomainInput{
		DomainName: aws.String(d.cfg.SearchDomain),
	})
	if err != nil || out.DomainStatus == nil {
		st.Status = describeErr(err, "domain not found")
		return ""
	}

	ds := out.DomainStatus
	created, processing := aws.ToBool(ds.Created), aws.ToBool(ds.Processing)
	switch {
	case processing:
		st.Status = "processing"
	case created:
		st.Status = "active"
	default:
		st.Status = "pending"
	}

	st.Advertised = aws.ToString(ds.Endpoint)

	// Floci advertises the OpenSearch endpoint as a Docker container name, which
	// resolves only from inside its Docker network. That is true when this
	// process is a container on that network (local compose) and false when it
	// is a Kubernetes pod. Rather than making the operator configure which world
	// they are in, just check whether the name resolves.
	switch {
	case d.cfg.SearchOverride != "":
		st.Effective = d.cfg.SearchOverride
		st.Note = "Using OPENSEARCH_ENDPOINT_OVERRIDE, set explicitly."

	case d.hostResolves(st.Advertised):
		st.Effective = st.Advertised
		st.Note = "Advertised Docker network name resolves from here, so it is used as-is."

	default:
		st.Effective = d.derivedSearchEndpoint()
		st.Note = fmt.Sprintf(
			"Advertised name %q does not resolve from here - it is a Docker network name and this is a Kubernetes pod. "+
				"Falling back to the port the OpenSearch container publishes on the pod IP.",
			hostOf(st.Advertised))
	}

	st.Ready = created && !processing && st.Effective != ""
	if !st.Ready {
		return ""
	}
	return st.Effective
}

func describeErr(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	// Keep it short: the full smithy error is noisy in a status panel.
	if len(err.Error()) > 120 {
		return err.Error()[:120] + "..."
	}
	return err.Error()
}

// hostResolves reports whether the host in a URL can be resolved from this
// process. The answer is cached: probeOpenSearch runs on every status request,
// and the answer cannot change without the pod being replaced.
func (d *Discoverer) hostResolves(rawURL string) bool {
	host := hostOf(rawURL)
	if host == "" {
		return false
	}

	if v, ok := d.resolvable.Load(host); ok {
		return v.(bool)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := net.DefaultResolver.LookupHost(ctx, host)
	ok := err == nil
	d.resolvable.Store(host, ok)
	if !ok {
		slog.Info("advertised OpenSearch host does not resolve; using the published port instead",
			"host", host)
	}
	return ok
}

// derivedSearchEndpoint points at the Floci service on the port the OpenSearch
// container publishes. Both containers share the pod's network namespace, so
// that port lands on the pod IP and the Floci Service already routes to it -
// which is why this needs no extra Kubernetes object.
func (d *Discoverer) derivedSearchEndpoint() string {
	host := hostOf(d.cfg.FlociEndpoint)
	if host == "" {
		host = "floci"
	}
	return fmt.Sprintf("http://%s:%d", host, d.cfg.SearchPublishedPort)
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}
