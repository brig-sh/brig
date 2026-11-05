package cilium

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// An empty selector must reach Cilium as {} and an allow default as
// egress: false. Left out, the first selects nothing and the second reads
// as true.
func TestNetworkPolicyMarshalsEmptySelectorAndFalseDefaultDeny(t *testing.T) {
	no := false
	np := NetworkPolicy{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   ObjectMeta{Name: "probe"},
		Spec: Spec{
			EndpointSelector:  EndpointSelector{MatchLabels: map[string]string{}},
			EnableDefaultDeny: DefaultDeny{Egress: &no},
			Egress: []EgressRule{{
				ToEndpoints: []EndpointSelector{{MatchLabels: map[string]string{
					"k8s:io.kubernetes.pod.namespace": "kube-system",
					"k8s:k8s-app":                     "kube-dns",
				}}},
				ToPorts: []PortRule{{
					Ports: []PortProtocol{{Port: DNSPort, Protocol: "ANY"}},
					Rules: &L7Rules{DNS: []DNSRule{{MatchPattern: "*"}}},
				}},
			}},
		},
	}
	out, err := yaml.Marshal(np)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"endpointSelector: {}",
		"egress: false",
		`port: "53"`,
		"protocol: ANY",
		"- matchPattern: '*'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("YAML lacks %q:\n%s", want, got)
		}
	}
	t.Logf("\n%s", got)
}
