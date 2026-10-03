package transform

import (
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func project() *types.Project {
	api := types.ServiceConfig{
		Name:       "api",
		Extensions: types.Extensions{"x-bouncer": map[string]any{}},
	}
	api.Image = "registry/api@sha256:1"
	api.Ports = []types.ServicePortConfig{{Target: 8080, Published: "8080", HostIP: "127.0.0.1", Protocol: "tcp"}}
	api.Networks = map[string]*types.ServiceNetworkConfig{"default": {Aliases: []string{"public-api"}}}
	worker := types.ServiceConfig{Name: "worker"}
	worker.Image = "registry/worker@sha256:2"
	worker.DependsOn = types.DependsOnConfig{"api": {Condition: "service_started"}}
	return &types.Project{Name: "proj", Services: types.Services{"api": api, "worker": worker}}
}

func TestProxyTakesNamePortsAndAliases(t *testing.T) {
	r, err := Apply(project())
	if err != nil {
		t.Fatal(err)
	}
	proxy := r.Project.Services["api"]
	if proxy.Image != "envoyproxy/envoy:v1.39.1" || len(proxy.Ports) != 1 || proxy.Ports[0].Published != "8080" {
		t.Fatalf("proxy: %+v", proxy)
	}
	if proxy.Networks["default"] == nil || proxy.Networks["default"].Aliases[0] != "public-api" {
		t.Fatal("proxy must keep the service's network aliases")
	}
	if proxy.Labels[LabelRole] != RoleProxy || proxy.Labels[LabelManaged] != "true" || proxy.Labels[LabelAdminPort] != "9901" {
		t.Fatalf("labels %v", proxy.Labels)
	}
	if proxy.Restart != types.RestartPolicyUnlessStopped || proxy.HealthCheck == nil {
		t.Fatal("proxy restart policy / healthcheck missing")
	}
	if !strings.Contains(*proxy.Environment["BOUNCER_CDS"], `"address":"api-app"`) {
		t.Fatal("proxy must be seeded with the replicas' service alias")
	}
}

func TestReplicasLoseHostPortsAndAliases(t *testing.T) {
	r, _ := Apply(project())
	app := r.Project.Services["api-app"]
	if app.Image != "registry/api@sha256:1" || len(app.Ports) != 0 {
		t.Fatalf("app: %+v", app)
	}
	if n := app.Networks["default"]; n == nil || len(n.Aliases) != 0 {
		t.Fatal("replicas keep the network but not the alias")
	}
	if app.Labels[LabelRole] != RoleReplica || app.Labels[LabelService] != "api" {
		t.Fatalf("labels %v", app.Labels)
	}
	if _, ok := app.Extensions["x-bouncer"]; ok {
		t.Fatal("x-bouncer must not be carried to replicas")
	}
}

func TestPlainServicesPassThrough(t *testing.T) {
	r, _ := Apply(project())
	if w := r.Project.Services["worker"]; w.DependsOn["api"].Condition != "service_started" {
		t.Fatal("depends_on on a Service now points at its proxy, unchanged")
	}
	if len(r.Services) != 1 || r.Services[0].Name != "api" {
		t.Fatalf("services %+v", r.Services)
	}
}

func TestTransformIsDeterministic(t *testing.T) {
	a, _ := Apply(project())
	b, _ := Apply(project())
	ya, _ := a.Project.MarshalYAML()
	yb, _ := b.Project.MarshalYAML()
	if string(ya) != string(yb) {
		t.Fatal("derived project differs between runs")
	}
}

func TestAppNameCollision(t *testing.T) {
	p := project()
	clash := types.ServiceConfig{Name: "api-app"}
	clash.Image = "x"
	p.Services["api-app"] = clash
	if _, err := Apply(p); err == nil || !strings.Contains(err.Error(), "api-app") {
		t.Fatalf("got %v", err)
	}
}

// The proxy's definition (and so its Compose config hash) ignores health
// config, which is applied in place; ports still change it.
func TestProxyDefinitionStableAcrossHealthConfig(t *testing.T) {
	proxyYAML := func(p *types.Project) string {
		r, err := Apply(p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := (&types.Project{Name: "x", Services: types.Services{"api": r.Project.Services["api"]}}).MarshalYAML()
		return string(b)
	}
	base := proxyYAML(project())
	hc := project()
	api := hc.Services["api"]
	api.Extensions = types.Extensions{"x-bouncer": map[string]any{"healthcheck": map[string]any{"uri": "/health"}}}
	hc.Services["api"] = api
	if got := proxyYAML(hc); got != base {
		t.Fatalf("health config changed the proxy:\n%s\n---\n%s", base, got)
	}
	ports := project()
	api = ports.Services["api"]
	api.Ports = []types.ServicePortConfig{{Target: 9090, Published: "8080", HostIP: "127.0.0.1", Protocol: "tcp"}}
	ports.Services["api"] = api
	if proxyYAML(ports) == base {
		t.Fatal("a port change must change the proxy")
	}
}

func TestDependentsAlsoDependOnReplicas(t *testing.T) {
	in := project()
	in.Services["api"] = func(s types.ServiceConfig) types.ServiceConfig {
		s.DependsOn = types.DependsOnConfig{"worker": {Condition: "service_started", Required: true}}
		return s
	}(in.Services["api"])
	db := types.ServiceConfig{Name: "web", Extensions: types.Extensions{"x-bouncer": map[string]any{}}}
	db.Image = "registry/web@sha256:3"
	db.Expose = types.StringOrNumberList{"80"}
	dep := types.ServiceDependency{Condition: "service_healthy", Restart: true, Required: true}
	db.DependsOn = types.DependsOnConfig{"api": dep}
	in.Services["web"] = db
	r, err := Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Project.Services
	if w := s["worker"].DependsOn; len(w) != 2 || !reflect.DeepEqual(w["api-app"], w["api"]) {
		t.Fatalf("plain dependent: %v", w)
	}
	if a := s["web-app"].DependsOn; len(a) != 2 || !reflect.DeepEqual(a["api"], dep) || !reflect.DeepEqual(a["api-app"], dep) {
		t.Fatalf("replica dependent: %v", a)
	}
	if a := s["api-app"].DependsOn; len(a) != 1 || a["worker"].Condition != "service_started" {
		t.Fatalf("a dependency on a plain service gets nothing added: %v", a)
	}
	if s["web"].DependsOn != nil || s["api"].DependsOn != nil {
		t.Fatal("proxies get no depends_on")
	}
	if len(in.Services["worker"].DependsOn) != 1 || len(in.Services["web"].DependsOn) != 1 {
		t.Fatal("the input project must not change")
	}
}
