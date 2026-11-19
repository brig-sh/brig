package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/brig-sh/brig/internal/policy/cilium"
	"github.com/brig-sh/brig/internal/wrap"
	"sigs.k8s.io/yaml"
)

// renderCiliumPolicy prints the CiliumNetworkPolicy for the egress section of
// one policy file.
//
// The file is read from the path given, not looked up by name, so a policy
// that is not installed can be rendered. The object name defaults to the
// policy's metadata.name.
func renderCiliumPolicy(args []string) error {
	var o cilium.Options
	fs := flag.NewFlagSet("render-cilium", flag.ContinueOnError)
	fs.StringVar(&o.Name, "name", "", "")
	fs.StringVar(&o.Namespace, "namespace", "", "")
	fs.BoolVar(&o.PodIdentity, "pod-identity", false, "")
	fs.BoolVar(&o.Cluster, "cluster", false, "")
	fs.StringVar(&o.BrokerCIDR, "broker-address", "", "")
	fs.Func("dns-search", "", func(v string) error {
		o.DNSSearch = append(o.DNSSearch, v)
		return nil
	})
	sel := map[string]string{}
	var selErr error
	fs.Func("selector", "", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			selErr = usagef("selector %q is not key=value", v)
			return selErr
		}
		sel[k] = val
		return nil
	})
	words, err := parseWords("render-cilium", "--name, --namespace, --selector, --pod-identity, --cluster, --broker-address and --dns-search", fs, args)
	if err != nil {
		if selErr != nil {
			return selErr
		}
		return err
	}
	switch {
	case len(words) == 0:
		return errors.New("policy render-cilium needs a file, for example `brig policy render-cilium policy.yaml`")
	case len(words) > 1:
		return fmt.Errorf("policy render-cilium takes one file, not %q", words[1])
	}
	if len(sel) > 0 {
		o.PodSelector = sel
	}
	if err := o.Check(); err != nil {
		return usagef("%v", err)
	}
	p, err := readPolicyFile(words[0])
	if err != nil {
		return err
	}
	np, err := cilium.Render(&p, o)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(np)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return err
	}
	// Cilium joins the allow rules of every policy that selects a pod, so
	// another policy that allows the host lets it through.
	for _, h := range np.DroppedDenyHosts() {
		wrap.Stderr.Say(fmt.Sprintf("deny host %q is left out: the object denies it only by not allowing it, so it stays denied only while no other CiliumNetworkPolicy for these pods allows it", h))
	}
	return nil
}
