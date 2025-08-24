package reschedule

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	checkpointDir             = util.ManagerRootPath + "/" + util.Checkpoints
	recoveryPodCheckpointFile = "recovery-checkpoint.json"
)

type recoveryCheckpoint struct {
	mut               sync.Mutex
	checkpointManager checkpointmanager.CheckpointManager
}

func newRecoveryCheckpoint() (*recoveryCheckpoint, error) {
	checkpointManager, err := checkpointmanager.NewCheckpointManager(checkpointDir)
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}
	checkpoints, err := checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}
	if !slices.Contains(checkpoints, recoveryPodCheckpointFile) {
		checkpoint := newCheckpoint()
		if err = checkpointManager.CreateCheckpoint(recoveryPodCheckpointFile, checkpoint); err != nil {
			return nil, fmt.Errorf("unable to sync to recovery checkpoint: %v", err)
		}
	}
	return &recoveryCheckpoint{
		checkpointManager: checkpointManager,
	}, nil
}

func (c *recoveryCheckpoint) ListPod() ([]*corev1.Pod, error) {
	c.mut.Lock()
	defer c.mut.Unlock()

	checkpoint, err := c.getOrCreateCheckpoint()
	if err != nil {
		return nil, err
	}
	pods := make([]*corev1.Pod, 0)
	for _, pod := range checkpoint.V1.RecoveryPods {
		pods = append(pods, pod.DeepCopy())
	}
	return pods, nil
}

func (c *recoveryCheckpoint) AddPod(pod *corev1.Pod) error {
	c.mut.Lock()
	defer c.mut.Unlock()

	checkpoint, err := c.getOrCreateCheckpoint()
	if err != nil {
		return err
	}
	podMap := checkpoint.V1.RecoveryPods
	podKey := client.ObjectKeyFromObject(pod).String()
	podMap[podKey] = *pod.DeepCopy()
	return c.checkpointManager.CreateCheckpoint(recoveryPodCheckpointFile, checkpoint)
}

func (c *recoveryCheckpoint) getOrCreateCheckpoint() (*Checkpoint, error) {
	checkpoint := newCheckpoint()
	err := c.checkpointManager.GetCheckpoint(recoveryPodCheckpointFile, checkpoint)
	if err != nil && err == errors.ErrCheckpointNotFound {
		err = c.checkpointManager.CreateCheckpoint(recoveryPodCheckpointFile, checkpoint)
	}
	return checkpoint, err
}

func (c *recoveryCheckpoint) RemovePod(podKey string) error {
	c.mut.Lock()
	defer c.mut.Unlock()

	checkpoint, err := c.getOrCreateCheckpoint()
	if err != nil {
		return err
	}
	delete(checkpoint.V1.RecoveryPods, podKey)
	return c.checkpointManager.CreateCheckpoint(recoveryPodCheckpointFile, checkpoint)
}

type Checkpoint struct {
	Checksum checksum.Checksum `json:"checksum"`
	V1       *CheckpointV1     `json:"v1,omitempty"`
}

type RecoveryPods map[string]corev1.Pod

type CheckpointV1 struct {
	RecoveryPods RecoveryPods `json:"recoveryPods,omitempty"`
}

func newCheckpoint() *Checkpoint {
	return &Checkpoint{
		Checksum: 0,
		V1: &CheckpointV1{
			RecoveryPods: make(RecoveryPods),
		},
	}
}

func (cp *Checkpoint) MarshalCheckpoint() ([]byte, error) {
	cp.Checksum = 0
	out, err := json.Marshal(*cp)
	if err != nil {
		return nil, err
	}
	cp.Checksum = checksum.New(out)
	return json.Marshal(*cp)
}

func (cp *Checkpoint) UnmarshalCheckpoint(data []byte) error {
	return json.Unmarshal(data, cp)
}

func (cp *Checkpoint) VerifyChecksum() error {
	ck := cp.Checksum
	cp.Checksum = 0
	defer func() {
		cp.Checksum = ck
	}()
	out, err := json.Marshal(*cp)
	if err != nil {
		return err
	}
	return ck.Verify(out)
}
