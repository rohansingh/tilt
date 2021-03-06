package engine

import (
	"archive/tar"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/windmilleng/tilt/internal/container"
	"github.com/windmilleng/tilt/internal/docker"
	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/k8s/testyaml"
	"github.com/windmilleng/tilt/internal/model"
	"github.com/windmilleng/tilt/internal/store"
	"github.com/windmilleng/tilt/internal/testutils"
	"github.com/windmilleng/tilt/internal/testutils/output"
	"github.com/windmilleng/tilt/internal/testutils/tempdir"
	"github.com/windmilleng/wmclient/pkg/dirs"
)

func TestStaticDockerfileWithCache(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoStaticManifestWithCache([]string{"/root/.cache"})
	cache := "gcr.io/some-project-162817/sancho:tilt-cache-3de427a264f80719a58a9abd456487b3"
	f.docker.Images[cache] = types.ImageInspect{}

	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `FROM gcr.io/some-project-162817/sancho:tilt-cache-3de427a264f80719a58a9abd456487b3
LABEL "tilt.cache"="0"
ADD . .
RUN go install github.com/windmilleng/sancho
ENTRYPOINT /go/bin/sancho
`,
	}
	testutils.AssertFileInTar(t, tar.NewReader(f.docker.BuildOptions.Context), expected)
}

func TestBaseDockerfileWithCache(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoFastBuildManifestWithCache(f, []string{"/root/.cache"})
	cache := "gcr.io/some-project-162817/sancho:tilt-cache-3de427a264f80719a58a9abd456487b3"
	f.docker.Images[cache] = types.ImageInspect{}

	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `FROM gcr.io/some-project-162817/sancho:tilt-cache-3de427a264f80719a58a9abd456487b3
LABEL "tilt.cache"="0"
ADD . /
RUN ["go", "install", "github.com/windmilleng/sancho"]
ENTRYPOINT ["/go/bin/sancho"]
LABEL "tilt.buildMode"="scratch"`,
	}
	testutils.AssertFileInTar(t, tar.NewReader(f.docker.BuildOptions.Context), expected)
}

func TestDeployTwinImages(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	sancho := NewSanchoFastBuildManifest(f)
	manifest := sancho.WithDeployTarget(sancho.K8sTarget().AppendYAML(SanchoTwinYAML))
	result, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	id := manifest.ImageTargetAt(0).ID()
	expectedImage := "gcr.io/some-project-162817/sancho:tilt-11cd0b38bc3ceb95"
	assert.Equal(t, expectedImage, result[id].Image.String())
	assert.Equalf(t, 2, strings.Count(f.k8s.Yaml, expectedImage),
		"Expected image to update twice in YAML: %s", f.k8s.Yaml)
}

func TestDeployPodWithMultipleImages(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	iTarget1 := NewSanchoStaticImageTarget()
	iTarget2 := NewSanchoSidecarStaticImageTarget()
	kTarget := model.K8sTarget{Name: "sancho", YAML: testyaml.SanchoSidecarYAML}.
		WithDependencyIDs([]model.TargetID{iTarget1.ID(), iTarget2.ID()})
	targets := []model.TargetSpec{iTarget1, iTarget2, kTarget}

	result, err := f.ibd.BuildAndDeploy(f.ctx, f.st, targets, store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, 2, f.docker.BuildCount)

	expectedSanchoRef := "gcr.io/some-project-162817/sancho:tilt-11cd0b38bc3ceb95"
	assert.Equal(t, expectedSanchoRef, result[iTarget1.ID()].Image.String())
	assert.Equalf(t, 1, strings.Count(f.k8s.Yaml, expectedSanchoRef),
		"Expected image to appear once in YAML: %s", f.k8s.Yaml)

	expectedSidecarRef := "gcr.io/some-project-162817/sancho-sidecar:tilt-11cd0b38bc3ceb95"
	assert.Equal(t, expectedSidecarRef, result[iTarget2.ID()].Image.String())
	assert.Equalf(t, 1, strings.Count(f.k8s.Yaml, expectedSidecarRef),
		"Expected image to appear once in YAML: %s", f.k8s.Yaml)
}

func TestDeployIDInjectedAndSent(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoStaticManifest()

	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	var deployID model.DeployID
	for _, a := range f.st.Actions {
		if deployIDAction, ok := a.(DeployIDAction); ok {
			deployID = deployIDAction.DeployID
		}
	}
	if deployID == 0 {
		t.Errorf("didn't find DeployIDAction w/ non-zero DeployID in actions: %v", f.st.Actions)
	}

	assert.True(t, strings.Count(f.k8s.Yaml, k8s.TiltDeployIDLabel) >= 1,
		"Expected TiltDeployIDLabel to appear at least once in YAML: %s", f.k8s.Yaml)
	assert.True(t, strings.Count(f.k8s.Yaml, deployID.String()) >= 1,
		"Expected DeployID %q to appear at least once in YAML: %s", deployID, f.k8s.Yaml)
}

func TestNoImageTargets(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	targName := "some-k8s-manifest"
	specs := []model.TargetSpec{
		model.K8sTarget{
			Name: model.TargetName(targName),
			YAML: testyaml.LonelyPodYAML,
		},
	}

	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, specs, store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, 0, f.docker.BuildCount, "expect no docker builds")
	assert.Equalf(t, 1, strings.Count(f.k8s.Yaml, "image: gcr.io/windmill-public-containers/lonely-pod"),
		"Expected lonely-pod image to appear once in YAML: %s", f.k8s.Yaml)

	expectedLabelStr := fmt.Sprintf("%s: %s", k8s.ManifestNameLabel, targName)
	assert.Equalf(t, 1, strings.Count(f.k8s.Yaml, expectedLabelStr),
		"Expected \"%s\"image to appear once in YAML: %s", expectedLabelStr, f.k8s.Yaml)
}

func TestMultiStageStaticBuild(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoStaticMultiStageManifest()
	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, 2, f.docker.BuildCount)
	assert.Equal(t, 1, f.docker.PushCount)

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `
FROM docker.io/library/sancho-base:tilt-11cd0b38bc3ceb95
ADD . .
RUN go install github.com/windmilleng/sancho
ENTRYPOINT /go/bin/sancho
`,
	}
	testutils.AssertFileInTar(t, tar.NewReader(f.docker.BuildOptions.Context), expected)
}

func TestMultiStageStaticBuildWithOnlyOneDirtyImage(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoStaticMultiStageManifest()
	iTargetID := manifest.ImageTargets[0].ID()
	result := store.NewImageBuildResult(iTargetID, container.MustParseNamedTagged("sancho-base:tilt-prebuilt"))
	state := store.NewBuildState(result, nil)
	stateSet := store.BuildStateSet{iTargetID: state}
	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), stateSet)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, 1, f.docker.BuildCount)

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `
FROM docker.io/library/sancho-base:tilt-prebuilt
ADD . .
RUN go install github.com/windmilleng/sancho
ENTRYPOINT /go/bin/sancho
`,
	}
	testutils.AssertFileInTar(t, tar.NewReader(f.docker.BuildOptions.Context), expected)
}

func TestMultiStageFastBuild(t *testing.T) {
	f := newIBDFixture(t)
	defer f.TearDown()

	manifest := NewSanchoFastMultiStageManifest(f)
	_, err := f.ibd.BuildAndDeploy(f.ctx, f.st, buildTargets(manifest), store.BuildStateSet{})
	if err != nil {
		t.Fatal(err)
	}

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `FROM docker.io/library/sancho-base:tilt-11cd0b38bc3ceb95

ADD . /
RUN ["go", "install", "github.com/windmilleng/sancho"]
ENTRYPOINT ["/go/bin/sancho"]
LABEL "tilt.buildMode"="scratch"`,
	}
	testutils.AssertFileInTar(t, tar.NewReader(f.docker.BuildOptions.Context), expected)
}

type ibdFixture struct {
	*tempdir.TempDirFixture
	ctx    context.Context
	docker *docker.FakeClient
	k8s    *k8s.FakeK8sClient
	ibd    *ImageBuildAndDeployer
	st     *store.TestingStore
}

func newIBDFixture(t *testing.T) *ibdFixture {
	f := tempdir.NewTempDirFixture(t)
	dir := dirs.NewWindmillDirAt(f.Path())
	docker := docker.NewFakeClient()
	ctx := output.CtxForTest()
	kClient := k8s.NewFakeK8sClient()
	ibd, err := provideImageBuildAndDeployer(ctx, docker, kClient, k8s.EnvGKE, dir)
	if err != nil {
		t.Fatal(err)
	}
	return &ibdFixture{
		TempDirFixture: f,
		ctx:            ctx,
		docker:         docker,
		k8s:            kClient,
		ibd:            ibd,
		st:             store.NewTestingStore(),
	}
}
