/*
Copyright 2017 The Kubernetes Authors.

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

package discovery

import (
	"fmt"
	"hash/fnv"
	"path/filepath"

	"github.com/golang/glog"
	"github.com/kubernetes-incubator/external-storage/local-volume/provisioner/pkg/common"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/api/v1/helper"
)

// Discoverer finds available volumes and creates PVs for them
// It looks for volumes in the directories specified in the discoveryMap
type Discoverer struct {
	*common.RuntimeConfig
	nodeAffinityAnn string
}

// NewDiscoverer creates a Discoverer object that will scan through
// the configured directories and create local PVs for any new directories found
func NewDiscoverer(config *common.RuntimeConfig) (*Discoverer, error) {
	affinity, err := generateNodeAffinity(config.Node)
	if err != nil {
		return nil, fmt.Errorf("Failed to generate node affinity: %v", err)
	}
	tmpAnnotations := map[string]string{}
	err = helper.StorageNodeAffinityToAlphaAnnotation(tmpAnnotations, affinity)
	if err != nil {
		return nil, fmt.Errorf("Failed to convert node affinity to alpha annotation: %v", err)
	}
	return &Discoverer{RuntimeConfig: config, nodeAffinityAnn: tmpAnnotations[v1.AlphaStorageNodeAffinityAnnotation]}, nil
}

func generateNodeAffinity(node *v1.Node) (*v1.NodeAffinity, error) {
	if node.Labels == nil {
		return nil, fmt.Errorf("Node does not have labels")
	}
	nodeValue, found := node.Labels[common.NodeLabelKey]
	if !found {
		return nil, fmt.Errorf("Node does not have expected label %s", common.NodeLabelKey)
	}

	return &v1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
			NodeSelectorTerms: []v1.NodeSelectorTerm{
				{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{
							Key:      common.NodeLabelKey,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{nodeValue},
						},
					},
				},
			},
		},
	}, nil
}

// DiscoverLocalVolumes reads the configured discovery paths, and creates PVs for the new volumes
func (d *Discoverer) DiscoverLocalVolumes() {
	for class, config := range d.DiscoveryMap {
		d.discoverVolumesAtPath(class, config)
	}
}

func (d *Discoverer) discoverVolumesAtPath(class string, config common.MountConfig) {
	glog.V(7).Infof("Discovering volumes at hostpath %q, mount path %q for storage class %q", config.HostDir, config.MountDir, class)

	files, err := d.VolUtil.ReadDir(config.MountDir)
	if err != nil {
		glog.Errorf("Error reading directory: %v", err)
		return
	}

	backedPVs := make(map[string]struct{})
	// check for new disk/dir
	for _, file := range files {
		// Check if PV already exists for it
		pvName := generatePVName(file, d.Node.Name, class)
		backedPVs[pvName] = struct{}{}
		_, exists := d.Cache.GetPV(pvName)
		if exists {
			continue
		}

		filePath := filepath.Join(config.MountDir, file)
		volType, err := d.getVolumeType(filePath)
		if err != nil {
			glog.Error(err)
			continue
		}

		var capacityByte int64
		switch volType {
		case common.VolumeTypeBlock:
			capacityByte, err = d.VolUtil.GetBlockCapacityByte(filePath)
			if err != nil {
				glog.Errorf("Path %q block stats error: %v", filePath, err)
				continue
			}
		case common.VolumeTypeFile:
			capacityByte, err = d.VolUtil.GetFsCapacityByte(filePath)
			if err != nil {
				glog.Errorf("Path %q fs stats error: %v", filePath, err)
				continue
			}
		default:
			glog.Errorf("Path %q has unexpected volume type %q", filePath, volType)
			continue
		}

		d.createPV(file, class, config, capacityByte, volType)
	}

	// cleanup removed disk/dir
	for _, pv := range d.Cache.ListPVs() {
		if _, ok := backedPVs[pv.Name]; ok {
			continue
		}
		if pv.Status.Phase == v1.VolumeBound {
			glog.Errorf("Missing backend storage media for pv %s", pv.Name)
		} else {
			d.deletePV(pv)
		}
	}
}

func (d *Discoverer) getVolumeType(fullPath string) (string, error) {
	isdir, errdir := d.VolUtil.IsDir(fullPath)
	if isdir {
		return common.VolumeTypeFile, nil
	}
	isblk, errblk := d.VolUtil.IsBlock(fullPath)
	if isblk {
		return common.VolumeTypeBlock, nil
	}

	return "", fmt.Errorf("Block device check for %q failed: DirErr - %v BlkErr - %v", fullPath, errdir, errblk)

}

func generatePVName(file, node, class string) string {
	h := fnv.New32a()
	h.Write([]byte(file))
	h.Write([]byte(node))
	h.Write([]byte(class))
	// This is the FNV-1a 32-bit hash
	return fmt.Sprintf("local-pv-%x", h.Sum32())
}

func (d *Discoverer) createPV(file, class string, config common.MountConfig, capacityByte int64, volType string) {
	pvName := generatePVName(file, d.Node.Name, class)
	outsidePath := filepath.Join(config.HostDir, file)

	glog.Infof("Found new volume of volumeType %q at host path %q with capacity %d, creating Local PV %q",
		volType, outsidePath, capacityByte, pvName)

	// TODO: Set block volumeType when the API is ready.
	pvSpec := common.CreateLocalPVSpec(&common.LocalPVConfig{
		Name:            pvName,
		HostPath:        outsidePath,
		Capacity:        capacityByte,
		StorageClass:    class,
		ProvisionerName: d.Name,
		AffinityAnn:     d.nodeAffinityAnn,
	})

	_, err := d.APIUtil.CreatePV(pvSpec)
	if err != nil {
		glog.Errorf("Error creating PV %q for volume at %q: %v", pvName, outsidePath, err)
		return
	}
	glog.Infof("Created PV %q for volume at %q", pvName, outsidePath)
}

func (d *Discoverer) deletePV(pv *v1.PersistentVolume) {
	err := d.APIUtil.DeletePV(pv.Name)
	if err != nil {
		deletingLocalPVErr := fmt.Errorf("Error deleting PV %q: %v", pv.Name, err.Error())
		d.RuntimeConfig.Recorder.Eventf(pv, v1.EventTypeWarning, common.EventVolumeFailedDelete, deletingLocalPVErr.Error())
		return
	}
	glog.Infof("Deleted PV %q", pv)
}
