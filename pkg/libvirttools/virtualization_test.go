/*
Copyright 2016-2017 Mirantis

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

package libvirttools

import (
	"io/ioutil"
	"os"
	"testing"
	"time"

	kubeapi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"

	"github.com/Mirantis/virtlet/pkg/bolttools"
	"github.com/Mirantis/virtlet/pkg/utils"
	"github.com/Mirantis/virtlet/pkg/virt/fake"
	"github.com/Mirantis/virtlet/tests/criapi"
	"github.com/Mirantis/virtlet/tests/gm"
)

const (
	fakeImageName = "fake/image1"
	fakeCNIConfig = `{"noCniForNow":true}`
)

type containerTester struct {
	t          *testing.T
	curTime    time.Time
	fakeTime   func() time.Time
	tmpDir     string
	sandbox    *kubeapi.PodSandboxConfig
	virtTool   *VirtualizationTool
	rec        *fake.TopLevelRecorder
	boltClient *bolttools.BoltClient
}

func newContainerTester(t *testing.T, rec *fake.TopLevelRecorder) *containerTester {
	ct := &containerTester{
		t:       t,
		curTime: time.Date(2017, 5, 30, 20, 19, 0, 0, time.UTC),
	}

	ct.fakeTime = func() time.Time { return ct.curTime }

	var err error
	ct.tmpDir, err = ioutil.TempDir("", "virtualization-test-")
	if err != nil {
		t.Fatalf("TempDir(): %v", err)
	}

	downloader := utils.NewFakeDownloader(ct.tmpDir)
	ct.rec = rec
	domainConn := fake.NewFakeDomainConnection(ct.rec.Child("domain conn"))
	storageConn := fake.NewFakeStorageConnection(ct.rec.Child("storage"))

	ct.boltClient, err = bolttools.NewFakeBoltClient()
	if err != nil {
		t.Fatalf("Failed to create fake bolt client: %v", err)
	}
	// TODO: uncomment this after moving image metadata handling to ImageTool
	// if err := boltClient.EnsureImageSchema(); err != nil {
	// 	t.Fatalf("boltClient: failed to create image schema: %v", err)
	// }
	if err := ct.boltClient.EnsureSandboxSchema(); err != nil {
		t.Fatalf("boltClient: failed to create sandbox schema: %v", err)
	}
	if err := ct.boltClient.EnsureVirtualizationSchema(); err != nil {
		t.Fatalf("boltClient: failed to create virtualization schema: %v", err)
	}

	imageTool, err := NewImageTool(storageConn, downloader, "default")
	if err != nil {
		t.Fatalf("Failed to create ImageTool: %v", err)
	}

	ct.virtTool, err = NewVirtualizationTool(domainConn, storageConn, imageTool, ct.boltClient, "volumes", "loop*")
	if err != nil {
		t.Fatalf("failed to create VirtualizationTool: %v", err)
	}
	ct.virtTool.SetTimeFunc(ct.fakeTime)
	// avoid unneeded difs in the golden master data
	ct.virtTool.SetForceKVM(true)

	// TODO: move image metadata store & name conversion to ImageTool
	// (i.e. methods like RemoveImage should accept image name)
	imageVolumeName, err := ImageNameToVolumeName(fakeImageName)
	if err != nil {
		t.Fatalf("Error getting volume name for image %q: %v", fakeImageName, err)
	}

	if _, err := imageTool.PullRemoteImageToVolume(fakeImageName, imageVolumeName); err != nil {
		t.Fatalf("Error pulling image %q to volume %q: %v", fakeImageName, imageVolumeName, err)
	}

	return ct
}

func (ct *containerTester) setPodSandbox(config *kubeapi.PodSandboxConfig) {
	if err := ct.boltClient.SetPodSandbox(config, []byte(fakeCNIConfig), ct.fakeTime); err != nil {
		ct.t.Fatalf("Failed to store pod sandbox: %v", err)
	}
}

func (ct *containerTester) elapse(d time.Duration) {
	ct.curTime = ct.curTime.Add(1 * time.Second)
}

func (ct *containerTester) teardown() {
	os.RemoveAll(ct.tmpDir)
}

func TestContainerLifecycle(t *testing.T) {
	ct := newContainerTester(t, fake.NewToplevelRecorder())
	defer ct.teardown()

	sandbox := criapi.GetSandboxes(1)[0]
	ct.setPodSandbox(sandbox)

	containers, err := ct.virtTool.ListContainers(nil)
	switch {
	case err != nil:
		t.Fatalf("ListContainers() failed: %v", err)
	case len(containers) != 0:
		t.Errorf("Unexpected containers when no containers are started: %#v", containers)
	}

	containerId, err := ct.virtTool.CreateContainer(&kubeapi.CreateContainerRequest{
		PodSandboxId: sandbox.Metadata.Uid,
		Config: &kubeapi.ContainerConfig{
			Metadata: &kubeapi.ContainerMetadata{
				Name: "container1",
			},
			Image: &kubeapi.ImageSpec{
				Image: fakeImageName,
			},
		},
		SandboxConfig: sandbox,
	}, "/tmp/fakenetns", fakeCNIConfig)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	containers, err = ct.virtTool.ListContainers(nil)
	switch {
	case err != nil:
		t.Errorf("ListContainers() failed: %v", err)
	case len(containers) != 1:
		t.Errorf("Expected single container to be started, but got: %#v", containers)
	case containers[0].Id != containerId:
		t.Errorf("Bad container id in response: %q instead of %q", containers[0].Id, containerId)
	case containers[0].State != kubeapi.ContainerState_CONTAINER_CREATED:
		t.Errorf("Bad container state: %v instead of %v", containers[0].State, kubeapi.ContainerState_CONTAINER_CREATED)
	}
	ct.rec.Rec("container list after the container is created", containers)

	ct.elapse(1 * time.Second)
	if err = ct.virtTool.StartContainer(containerId); err != nil {
		t.Fatalf("StartContainer failed for container %q: %v", containerId, err)
	}

	status, err := ct.virtTool.ContainerStatus(containerId)
	switch {
	case err != nil:
		t.Errorf("ContainerStatus(): %v", err)
	case status.State != kubeapi.ContainerState_CONTAINER_RUNNING:
		t.Errorf("Bad container state: %v instead of %v", containers[0].State, kubeapi.ContainerState_CONTAINER_RUNNING)
	}
	ct.rec.Rec("container status the container is created", status)

	if err = ct.virtTool.StopContainer(containerId); err != nil {
		t.Fatalf("StopContainer failed for container %q: %v", containerId, err)
	}

	status, err = ct.virtTool.ContainerStatus(containerId)
	switch {
	case err != nil:
		t.Errorf("ContainerStatus(): %v", err)
	case status.State != kubeapi.ContainerState_CONTAINER_EXITED:
		t.Errorf("Bad container state: %v instead of %v", containers[0].State, kubeapi.ContainerState_CONTAINER_EXITED)
	}
	ct.rec.Rec("container status the container is stopped", status)

	if err = ct.virtTool.RemoveContainer(containerId); err != nil {
		t.Fatalf("RemoveContainer failed for container %q: %v", containerId, err)
	}

	containers, err = ct.virtTool.ListContainers(nil)
	switch {
	case err != nil:
		t.Fatalf("ListContainers() failed: %v", err)
	case len(containers) != 0:
		t.Errorf("Unexpected containers when no containers are started: %#v", containers)
	}

	gm.Verify(t, ct.rec.Content())
}

func TestDomainDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{
			name: "plain domain",
		},
		{
			name: "raw devices",
			annotations: map[string]string{
				// FIXME: here we depend upon the fact that /dev/loop0
				// indeed exists in the build container. But we shouldn't.
				"VirtletVolumes": `[{"Name": "vol", "Format": "rawDevice", "Path": "/dev/loop0"}]`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := fake.NewToplevelRecorder()
			rec.AddFilter("DefineDomain")

			ct := newContainerTester(t, rec)
			defer ct.teardown()

			sandbox := criapi.GetSandboxes(1)[0]
			sandbox.Annotations = tc.annotations
			ct.setPodSandbox(sandbox)

			containerId, err := ct.virtTool.CreateContainer(&kubeapi.CreateContainerRequest{
				PodSandboxId: sandbox.Metadata.Uid,
				Config: &kubeapi.ContainerConfig{
					Metadata: &kubeapi.ContainerMetadata{
						Name: "container1",
					},
					Image: &kubeapi.ImageSpec{
						Image: fakeImageName,
					},
				},
				SandboxConfig: sandbox,
			}, "/tmp/fakenetns", fakeCNIConfig)
			if err != nil {
				t.Fatalf("CreateContainer: %v", err)
			}

			if err = ct.virtTool.RemoveContainer(containerId); err != nil {
				t.Fatalf("RemoveContainer failed for container %q: %v", containerId, err)
			}

			gm.Verify(t, ct.rec.Content())
		})
	}
}