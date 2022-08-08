/*
Copyright 2022 The KubeOne Authors.

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

package e2e

import (
	"context"
	"fmt"
	"io"
	"testing"
	"text/template"
	"time"

	"sigs.k8s.io/yaml"
)

const kubeoneVersionToInit = "1.4.6"

type scenarioUpgrade struct {
	Name                 string
	ManifestTemplatePath string

	versions []string
	infra    Infra
}

func (scenario scenarioUpgrade) Title() string { return titleize(scenario.Name) }

func (scenario *scenarioUpgrade) SetInfra(infra Infra) {
	scenario.infra = infra
}

func (scenario *scenarioUpgrade) SetVersions(versions ...string) {
	scenario.versions = versions
}

func (scenario *scenarioUpgrade) Run(t *testing.T) {
	if err := makeBin("build").Run(); err != nil {
		t.Fatalf("building kubeone: %v", err)
	}

	install := &scenarioInstall{
		Name:                 scenario.Name,
		ManifestTemplatePath: scenario.ManifestTemplatePath,
		infra:                scenario.infra,
		versions:             []string{scenario.versions[0]},
		kubeonePath:          downloadKubeone(t, kubeoneVersionToInit),
	}

	install.install(t)
	scenario.upgrade(t)
	scenario.test(t)
}

func (scenario *scenarioUpgrade) kubeone(t *testing.T, version string) *kubeoneBin {
	var k1Opts []kubeoneBinOpts

	if *kubeoneVerboseFlag {
		k1Opts = append(k1Opts, withKubeoneVerbose)
	}

	if *credentialsFlag != "" {
		k1Opts = append(k1Opts, withKubeoneCredentials(*credentialsFlag))
	}

	return newKubeoneBin(
		scenario.infra.terraform.path,
		renderManifest(t,
			scenario.ManifestTemplatePath,
			manifestData{
				VERSION: version,
			},
		),
		k1Opts...,
	)
}

func (scenario *scenarioUpgrade) upgrade(t *testing.T) {
	// NB: Due to changed node selectors between Kubernetes 1.23 and 1.24, it's
	// important to run apply with KubeOne 1.5 before upgrading the cluster,
	// otherwise upgrade might get stuck due to pods not able to get
	// rescheduled.
	k1 := scenario.kubeone(t, scenario.versions[0])
	if err := k1.Apply(); err != nil {
		t.Fatalf("kubeone apply failed: %v", err)
	}

	k1 = scenario.kubeone(t, scenario.versions[1])
	if err := k1.Apply(); err != nil {
		t.Fatalf("kubeone apply failed: %v", err)
	}
}

func (scenario *scenarioUpgrade) test(t *testing.T) {
	k1 := scenario.kubeone(t, scenario.versions[1])

	// launch kubeone proxy, to have a HTTPS proxy through the SSH tunnel
	// to open access to the kubeapi behind the bastion host
	proxyCtx, killProxy := context.WithCancel(context.Background())
	proxyURL, waitK1, err := k1.AsyncProxy(proxyCtx)
	if err != nil {
		t.Fatalf("starting kubeone proxy: %v", err)
	}
	defer func() {
		waitErr := waitK1()
		if waitErr != nil {
			t.Logf("wait kubeone proxy: %v", waitErr)
		}
	}()
	defer killProxy()

	// let kubeone proxy start and open the port
	time.Sleep(5 * time.Second)
	t.Logf("kubeone proxy is running on %s", proxyURL)

	waitKubeOneNodesReady(t, k1)

	client := dynamicClientRetriable(t, k1)
	cpTests := newCloudProviderTests(client, scenario.infra.Provider())
	cpTests.runWithCleanup(t)

	sonobuoyRun(t, k1, sonobuoyConformanceLite, proxyURL)
}

func (scenario *scenarioUpgrade) GenerateTests(wr io.Writer, generatorType GeneratorType, cfg ProwConfig) error {
	if len(scenario.versions) != 2 {
		return fmt.Errorf("expected only 2 versions")
	}

	type upgradeFromTo struct {
		From string
		To   string
	}

	up := upgradeFromTo{
		From: scenario.versions[0],
		To:   scenario.versions[1],
	}

	type templateData struct {
		Infra       string
		Scenario    string
		FromVersion string
		ToVersion   string
		TestTitle   string
	}

	var (
		data     []templateData
		prowJobs []ProwJob
	)

	testTitle := fmt.Sprintf("Test%s%sFrom%s_To%s",
		titleize(scenario.infra.name),
		scenario.Title(),
		titleize(up.From),
		titleize(up.To),
	)

	data = append(data, templateData{
		TestTitle:   testTitle,
		Infra:       scenario.infra.name,
		Scenario:    scenario.Name,
		FromVersion: up.From,
		ToVersion:   up.To,
	})

	cfg.Environ = scenario.infra.environ

	prowJobs = append(prowJobs,
		newProwJob(
			pullProwJobName(scenario.infra.name, scenario.Name, "from", up.From, "to", up.To),
			scenario.infra.labels,
			testTitle,
			cfg,
		),
	)

	switch generatorType {
	case GeneratorTypeGo:
		tpl, err := template.New("").Parse(upgradeScenarioTemplate)
		if err != nil {
			return err
		}

		return tpl.Execute(wr, data)
	case GeneratorTypeYAML:
		buf, err := yaml.Marshal(prowJobs)
		if err != nil {
			return err
		}

		n, err := wr.Write(buf)
		if err != nil {
			return err
		}

		if n != len(buf) {
			return fmt.Errorf("wrong number of bytes written, expected %d, wrote %d", len(buf), n)
		}

		return nil
	}

	return fmt.Errorf("unknown generator type %d", generatorType)
}

const upgradeScenarioTemplate = `
{{- range . }}
func {{ .TestTitle }}(t *testing.T) {
	infra := Infrastructures["{{ .Infra }}"]
	scenario := Scenarios["{{ .Scenario }}"]
	scenario.SetInfra(infra)
	scenario.SetVersions("{{ .FromVersion }}", "{{ .ToVersion }}")
	scenario.Run(t)
}
{{ end -}}
`