/*
Copyright 2021 Alibaba Group Holding Limited.

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

package rebalance

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/alibaba/polardbx-operator/pkg/k8s/control"
	"github.com/alibaba/polardbx-operator/pkg/meta/core/gms"
	"github.com/alibaba/polardbx-operator/pkg/meta/core/group"
	"github.com/alibaba/polardbx-operator/pkg/operator/v1/featuregate"
	"github.com/alibaba/polardbx-operator/pkg/operator/v1/polardbx/convention"
	polardbxv1reconcile "github.com/alibaba/polardbx-operator/pkg/operator/v1/polardbx/reconcile"
	"github.com/alibaba/polardbx-operator/pkg/operator/v1/polardbx/task"
)

type DataRebalanceTask struct {
	From          int      `json:"from,omitempty"`
	To            int      `json:"to,omitempty"`
	JobIds        []string `json:"job_ids,omitempty"`
	ShowMoveIndex int      `json:"show_move_index,omitempty"`
}

func (t *DataRebalanceTask) startRebalanceClusterForScaleOut(rc *polardbxv1reconcile.Context) ([]group.RebalanceAction, error) {
	groupMgr, err := rc.GetPolarDBXGroupManager()
	if err != nil {
		return nil, err
	}

	moveStatus, err := groupMgr.ShowMoveStatus("", t.ShowMoveIndex)
	if err != nil {
		return nil, err
	}
	// Consider it's in rebalancing if there's new move status after prepare.
	if len(moveStatus) > 0 {
		return nil, nil
	}

	return groupMgr.RebalanceCluster(t.To)
}

func (t *DataRebalanceTask) startDrainNodes(rc *polardbxv1reconcile.Context) ([]group.RebalanceAction, error) {
	polardbx := rc.MustGetPolarDBX()
	groupMgr, err := rc.GetPolarDBXGroupManager()
	if err != nil {
		return nil, err
	}

	toDrainDNs := make([]string, 0)
	for i := t.To; i < t.From; i++ {
		toDrainDNs = append(toDrainDNs, convention.NewDNName(polardbx, i))
	}

	return groupMgr.DrainStorageNodes(toDrainDNs...)
}

func (t *DataRebalanceTask) Skip() bool {
	return t.From > 0 && t.From == t.To
}

func (t *DataRebalanceTask) Start(rc *polardbxv1reconcile.Context) ([]group.RebalanceAction, error) {
	if t.From == t.To {
		return nil, nil
	} else if t.From < t.To { // Scale out
		if !featuregate.AutoDataRebalance.Enabled() {
			return nil, nil
		}
		return t.startRebalanceClusterForScaleOut(rc)
	} else { // Scale in
		return t.startDrainNodes(rc)
	}
}

func (t *DataRebalanceTask) getShowMoveDatabaseProgress(rc *polardbxv1reconcile.Context) (int, error) {
	groupMgr, err := rc.GetPolarDBXGroupManager()
	if err != nil {
		return 0, err
	}

	moveStatus, err := groupMgr.ShowMoveStatus("", t.ShowMoveIndex)
	if err != nil {
		return 0, err
	}

	if len(moveStatus) == 0 {
		return 100, nil
	}

	progress := 0
	for _, mv := range moveStatus {
		progress += int(mv.Progress)
	}

	return progress / len(moveStatus), nil
}

func (t *DataRebalanceTask) areScaleInDrainedNodesOffline(rc *polardbxv1reconcile.Context) (bool, error) {
	p, err := t.getScaleInProgressByCountingDrainedNodes(rc)
	if err != nil {
		return false, err
	}
	return p == 100, nil
}

func (t *DataRebalanceTask) getScaleInProgressByCountingDrainedNodes(rc *polardbxv1reconcile.Context) (int, error) {
	gmsMgr, err := rc.GetPolarDBXGMSManager()
	if err != nil {
		return 0, err
	}

	toDrainCnt := t.From - t.To

	if toDrainCnt == 0 {
		return 100, nil
	}

	storageNodes, err := gmsMgr.ListStorageNodes(gms.StorageKindMaster)
	if err != nil {
		return 0, err
	}

	drainedCnt := 0
	for _, node := range storageNodes {
		if node.Status == gms.PSNodeDisabled {
			drainedCnt++
		}
	}

	if drainedCnt > toDrainCnt {
		panic("never happens or bug")
	}

	return drainedCnt * 100 / toDrainCnt, nil
}

func (t *DataRebalanceTask) Progress(rc *polardbxv1reconcile.Context) (int, error) {
	if t.From == t.To {
		return 100, nil
	} else if t.From < t.To { // Scale out
		if !featuregate.AutoDataRebalance.Enabled() {
			return 100, nil
		}
		return t.getShowMoveDatabaseProgress(rc)
	} else {
		return t.getShowMoveDatabaseProgress(rc)
	}
}

func (t *DataRebalanceTask) IsReady(rc *polardbxv1reconcile.Context) (bool, error) {
	progress, err := t.Progress(rc)
	if err != nil {
		return false, err
	}
	return progress == 100, nil
}

var PrepareRebalanceTaskContext = polardbxv1reconcile.NewStepBinder("PrepareRebalanceTaskContext",
	func(rc *polardbxv1reconcile.Context, flow control.Flow) (reconcile.Result, error) {
		// Read task context from config map.
		taskCm, err := rc.GetPolarDBXConfigMap(convention.ConfigMapTypeTask)
		if err != nil {
			return flow.Error(err, "Unable to get config map for task.")
		}

		contextAccess := task.NewContextAccess(taskCm, "rebalance")
		rebalanceTask := &DataRebalanceTask{}
		ok, err := contextAccess.Read(rebalanceTask)
		if err != nil {
			return flow.Error(err, "Unable to read rebalance task context.")
		}
		if ok {
			return flow.Pass()
		}

		// Get target DN replicas
		polardbx := rc.MustGetPolarDBX()
		toReplicas := int(polardbx.Status.SpecSnapshot.Topology.Nodes.DN.Replicas)

		// Compare current
		gmsMgr, err := rc.GetPolarDBXGMSManager()
		if err != nil {
			return flow.Error(err, "Unable to get manager of GMS.")
		}
		storageNodes, err := gmsMgr.ListStorageNodes(gms.StorageKindMaster)
		if err != nil {
			return flow.Error(err, "Unable to list storages of DNs.")
		}

		fromReplicas := len(storageNodes)

		groupMgr, err := rc.GetPolarDBXGroupManager()
		if err != nil {
			return flow.Error(err, "Unable to get group manager.")
		}
		moveStatus, err := groupMgr.ShowMoveStatus("", 0)
		if err != nil {
			return flow.Error(err, "Unable to get move status.")
		}

		// Write task context into config map.
		rebalanceTask = &DataRebalanceTask{
			From:          fromReplicas,
			To:            toReplicas,
			ShowMoveIndex: len(moveStatus),
		}

		err = contextAccess.Write(rebalanceTask)
		if err != nil {
			return flow.Error(err, "Unable to write rebalance task into config map.")
		}

		// Update config map.
		err = rc.Client().Update(rc.Context(), taskCm)
		if err != nil {
			return flow.Error(err, "Unable to update task config map.")
		}

		return flow.Continue("Rebalance task context prepared.")
	},
)

var StartRebalanceTask = polardbxv1reconcile.NewStepBinder("StartRebalanceTask",
	func(rc *polardbxv1reconcile.Context, flow control.Flow) (reconcile.Result, error) {
		// Read task context from config map.
		taskCm, err := rc.GetPolarDBXConfigMap(convention.ConfigMapTypeTask)
		if err != nil {
			return flow.Error(err, "Unable to get config map for task.")
		}

		contextAccess := task.NewContextAccess(taskCm, "rebalance")
		rebalanceTask := &DataRebalanceTask{}
		ok, err := contextAccess.Read(rebalanceTask)
		if err != nil {
			return flow.Error(err, "Unable to read rebalance task context.")
		}
		if !ok {
			return flow.Error(errors.New("no rebalance task context found"), "Unable to find rebalance task context.")
		}

		// Skip immediately.
		if rebalanceTask.Skip() {
			return flow.Pass()
		}

		// Start a new task.
		rebalanceActions, err := rebalanceTask.Start(rc)
		if err != nil {
			return flow.Error(err, "Unable to start rebalance task.")
		}

		// Log actions.
		flow.Logger().Info("Rebalance actions started.", "rebalance-actions", rebalanceActions)

		return flow.Pass()
	},
)

func WatchRebalanceTaskAntUpdateProgress(interval time.Duration) control.BindFunc {
	return polardbxv1reconcile.NewStepBinder("WatchRebalanceTaskAntUpdateProgress",
		func(rc *polardbxv1reconcile.Context, flow control.Flow) (reconcile.Result, error) {
			// Read task context from config map.
			taskCm, err := rc.GetPolarDBXConfigMap(convention.ConfigMapTypeTask)
			if err != nil {
				return flow.Error(err, "Unable to get config map for task.")
			}

			contextAccess := task.NewContextAccess(taskCm, "rebalance")
			rebalanceTask := &DataRebalanceTask{}
			ok, err := contextAccess.Read(rebalanceTask)
			if err != nil {
				return flow.Error(err, "Unable to read rebalance task context.")
			}
			if !ok {
				return flow.Error(errors.New("no rebalance task context found"), "Unable to find rebalance task context.")
			}

			polardbx := rc.MustGetPolarDBX()

			// Skip immediately.
			if rebalanceTask.Skip() {
				polardbx.Status.StatusForPrint.RebalanceProcess = "skip"
				return flow.Continue("Skip rebalance.")
			}

			// Block if not ready.
			progress, err := rebalanceTask.Progress(rc)
			if err != nil {
				return flow.Error(err, "Unable to get progress of rebalance task.")
			}

			polardbx.Status.StatusForPrint.RebalanceProcess = fmt.Sprintf("%.1f%%", float64(progress))

			if progress < 100 {
				return flow.RequeueAfter(interval, "Rebalance not ready, wait for recheck.")
			} else {
				return flow.Pass()
			}
		},
	)
}

func IsTrailingDNsDrained(rc *polardbxv1reconcile.Context, rebalanceTask *DataRebalanceTask) (bool, error) {
	polardbx := rc.MustGetPolarDBX()

	drainedDNs := make(map[string]int, 0)
	for i := rebalanceTask.To; i < rebalanceTask.From; i++ {
		drainedDNs[convention.NewDNName(polardbx, i)] = 0
	}

	// Check for each schema.
	groupMgr, err := rc.GetPolarDBXGroupManager()
	if err != nil {
		return false, err
	}

	schemas, err := groupMgr.ListSchemas()
	if err != nil {
		return false, err
	}

	// Scan all schemas to ensure no group is on drained DNs.
	for _, schema := range schemas {
		// Skip system schemas.
		if strings.ToLower(schema) == "information_schema" {
			continue
		}

		groups, err := groupMgr.ListGroups(schema)
		if err != nil {
			return false, err
		}

		for _, grp := range groups {
			// Found group on to drain DN.
			if _, ok := drainedDNs[grp.StorageId]; ok {
				return false, nil
			}
		}
	}

	return true, nil
}

var EnsureTrailingDNsAreDrainedOrBlock = polardbxv1reconcile.NewStepBinder(
	"EnsureTrailingDNsAreDrainedOrRestartRebalance",
	func(rc *polardbxv1reconcile.Context, flow control.Flow) (reconcile.Result, error) {
		// Read task context from config map.
		taskCm, err := rc.GetPolarDBXConfigMap(convention.ConfigMapTypeTask)
		if err != nil {
			return flow.Error(err, "Unable to get config map for task.")
		}

		contextAccess := task.NewContextAccess(taskCm, "rebalance")
		rebalanceTask := &DataRebalanceTask{}
		ok, err := contextAccess.Read(rebalanceTask)
		if err != nil {
			return flow.Error(err, "Unable to read rebalance task context.")
		}
		if !ok {
			return flow.Error(errors.New("no rebalance task context found"), "Unable to find rebalance task context.")
		}

		drained, err := IsTrailingDNsDrained(rc, rebalanceTask)
		if err != nil {
			return flow.Error(err, "Unable to determine if trailing DNs are drained.")
		}

		// Block if CDC enabled or feature gate WaitDrainedNodeToBeOffline enabled.
		polardbx := rc.MustGetPolarDBX()
		cdcNodeSpec := polardbx.Status.SpecSnapshot.Topology.Nodes.CDC
		if featuregate.WaitDrainedNodeToBeOffline.Enabled() ||
			(cdcNodeSpec != nil && cdcNodeSpec.Replicas > 0) {
			offline, err := rebalanceTask.areScaleInDrainedNodesOffline(rc)
			if err != nil {
				return flow.Error(err, "Unable to determine offline status from GMS.")
			}
			if !offline {
				return flow.RequeueAfter(20*time.Second, "Block until trailing DNs are marked offline.")
			}
		}

		if drained {
			return flow.Pass()
		} else {
			polardbx := rc.MustGetPolarDBX()
			polardbx.Status.StatusForPrint.RebalanceProcess = "stuck"

			return flow.Wait("Trailing DNs are not drained, must be verified manually.")
		}
	},
)

var ResetRebalanceTask = polardbxv1reconcile.NewStepBinder("ResetRebalanceTask",
	func(rc *polardbxv1reconcile.Context, flow control.Flow) (reconcile.Result, error) {
		polardbx := rc.MustGetPolarDBX()
		polardbx.Status.StatusForPrint.RebalanceProcess = ""

		taskCm, err := rc.GetPolarDBXConfigMap(convention.ConfigMapTypeTask)
		if err != nil {
			return flow.Error(err, "Unable to get config map for task.")
		}

		contextAccess := task.NewContextAccess(taskCm, "rebalance")
		ok := contextAccess.Clear()

		// Update config map if cleared.
		if ok {
			err = rc.Client().Update(rc.Context(), taskCm)
			if err != nil {
				return flow.Error(err, "Unable to update task config map.")
			}
		}

		return flow.Pass()
	},
)
