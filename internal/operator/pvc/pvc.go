package pvc

/*
 Copyright 2017 - 2020 Crunchy Data Solutions, Inc.
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

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/crunchydata/postgres-operator/internal/config"
	"github.com/crunchydata/postgres-operator/internal/kubeapi"
	"github.com/crunchydata/postgres-operator/internal/operator"
	crv1 "github.com/crunchydata/postgres-operator/pkg/apis/crunchydata.com/v1"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type matchLabelsTemplateFields struct {
	Key   string
	Value string
}

// TemplateFields ...
type TemplateFields struct {
	Name         string
	AccessMode   string
	ClusterName  string
	Size         string
	StorageClass string
	MatchLabels  string
}

// CreateMissingPostgreSQLVolumes converts the storage specifications of cluster
// related to PostgreSQL into StorageResults. When a specification calls for a
// PVC to be created, the PVC is created unless it already exists.
func CreateMissingPostgreSQLVolumes(clientset *kubernetes.Clientset,
	cluster *crv1.Pgcluster, namespace string,
	pvcNamePrefix string, dataStorageSpec crv1.PgStorageSpec,
) (
	dataVolume, walVolume operator.StorageResult,
	tablespaceVolumes map[string]operator.StorageResult,
	err error,
) {
	dataVolume, err = CreateIfNotExists(clientset,
		dataStorageSpec, pvcNamePrefix, cluster.Spec.Name, namespace)

	if err == nil {
		walVolume, err = CreateIfNotExists(clientset,
			cluster.Spec.WALStorage, pvcNamePrefix+"-wal", cluster.Spec.Name, namespace)
	}

	tablespaceVolumes = make(map[string]operator.StorageResult, len(cluster.Spec.TablespaceMounts))
	for tablespaceName, storageSpec := range cluster.Spec.TablespaceMounts {
		if err == nil {
			tablespacePVCName := operator.GetTablespacePVCName(pvcNamePrefix, tablespaceName)
			tablespaceVolumes[tablespaceName], err = CreateIfNotExists(clientset,
				storageSpec, tablespacePVCName, cluster.Spec.Name, namespace)
		}
	}

	return
}

// CreateIfNotExists converts a storage specification into a StorageResult. If
// spec calls for a PVC to be created and pvcName does not exist, it will be created.
func CreateIfNotExists(clientset *kubernetes.Clientset, spec crv1.PgStorageSpec, pvcName, clusterName, namespace string) (operator.StorageResult, error) {
	result := operator.StorageResult{
		SupplementalGroups: spec.GetSupplementalGroups(),
	}

	switch spec.StorageType {
	case "", "emptydir":
		// no-op

	case "existing":
		result.PersistentVolumeClaimName = spec.Name

	case "create", "dynamic":
		result.PersistentVolumeClaimName = pvcName
		err := Create(clientset, pvcName, clusterName, &spec, namespace)
		if err != nil && !kubeapi.IsAlreadyExists(err) {
			log.Errorf("error in pvc create: %v", err)
			return result, err
		}
	}

	return result, nil
}

// CreatePVC create a pvc
func CreatePVC(clientset *kubernetes.Clientset, storageSpec *crv1.PgStorageSpec, pvcName, clusterName, namespace string) (string, error) {
	var err error

	switch storageSpec.StorageType {
	case "":
		log.Debug("StorageType is empty")
	case "emptydir":
		log.Debug("StorageType is emptydir")
	case "existing":
		log.Debug("StorageType is existing")
		pvcName = storageSpec.Name
	case "create", "dynamic":
		log.Debug("StorageType is create")
		log.Debugf("pvcname=%s storagespec=%v", pvcName, storageSpec)
		err = Create(clientset, pvcName, clusterName, storageSpec, namespace)
		if err != nil {
			log.Error("error in pvc create " + err.Error())
			return pvcName, err
		}
		log.Info("created PVC =" + pvcName + " in namespace " + namespace)
	}

	return pvcName, err
}

// Create a pvc
func Create(clientset *kubernetes.Clientset, name, clusterName string, storageSpec *crv1.PgStorageSpec, namespace string) error {
	log.Debug("in createPVC")
	var doc2 bytes.Buffer
	var err error

	pvcFields := TemplateFields{
		Name:         name,
		AccessMode:   storageSpec.AccessMode,
		StorageClass: storageSpec.StorageClass,
		ClusterName:  clusterName,
		Size:         storageSpec.Size,
		MatchLabels:  storageSpec.MatchLabels,
	}

	if storageSpec.StorageType == "dynamic" {
		log.Debug("using dynamic PVC template")
		err = config.PVCStorageClassTemplate.Execute(&doc2, pvcFields)
		if operator.CRUNCHY_DEBUG {
			config.PVCStorageClassTemplate.Execute(os.Stdout, pvcFields)
		}
	} else {
		log.Debugf("matchlabels from spec is [%s]", storageSpec.MatchLabels)
		if storageSpec.MatchLabels != "" {
			arr := strings.Split(storageSpec.MatchLabels, "=")
			if len(arr) != 2 {
				log.Errorf("%s MatchLabels is not formatted correctly", storageSpec.MatchLabels)
				return errors.New("match labels is not formatted correctly")
			}
			pvcFields.MatchLabels = getMatchLabels(arr[0], arr[1])
			log.Debugf("matchlabels constructed is %s", pvcFields.MatchLabels)
		}

		err = config.PVCTemplate.Execute(&doc2, pvcFields)
		if operator.CRUNCHY_DEBUG {
			config.PVCTemplate.Execute(os.Stdout, pvcFields)
		}
	}
	if err != nil {
		log.Error("error in pvc create exec" + err.Error())
		return err
	}

	newpvc := v1.PersistentVolumeClaim{}
	err = json.Unmarshal(doc2.Bytes(), &newpvc)
	if err != nil {
		log.Error("error unmarshalling json into PVC " + err.Error())
		return err
	}

	_, err = clientset.CoreV1().PersistentVolumeClaims(namespace).Create(&newpvc)
	return err
}

// Delete a pvc
func DeleteIfExists(clientset *kubernetes.Clientset, name string, namespace string) error {
	pvc, err := kubeapi.GetPVCIfExists(clientset, name, namespace)
	if pvc == nil {
		// nothing to delete. return any other error.
		return err
	}

	log.Debugf("PVC %s is found", pvc.Name)

	if pvc.ObjectMeta.Labels[config.LABEL_PGREMOVE] == "true" {
		log.Debugf("delete PVC %s in namespace %s", name, namespace)
		err = kubeapi.DeletePVC(clientset, name, namespace)
	}
	return err
}

// Exists test to see if pvc exists
func Exists(clientset *kubernetes.Clientset, name string, namespace string) bool {
	pvc, _ := kubeapi.GetPVCIfExists(clientset, name, namespace)
	return pvc != nil
}

func getMatchLabels(key, value string) string {

	matchLabelsTemplateFields := matchLabelsTemplateFields{}
	matchLabelsTemplateFields.Key = key
	matchLabelsTemplateFields.Value = value

	var doc bytes.Buffer
	err := config.PVCMatchLabelsTemplate.Execute(&doc, matchLabelsTemplateFields)
	if err != nil {
		log.Error(err.Error())
		return ""
	}

	return doc.String()

}
