/*


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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scansv1 "experimental.securecodebox.io/api/v1"
	"github.com/minio/minio-go/v6"
)

var (
	ownerKey = ".metadata.controller"
	apiGVStr = scansv1.GroupVersion.String()
)

// ScanReconciler reconciles a Scan object
type ScanReconciler struct {
	client.Client
	Log         logr.Logger
	Scheme      *runtime.Scheme
	MinioClient minio.Client
}

// +kubebuilder:rbac:groups=scans.experimental.securecodebox.io,resources=scans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scans.experimental.securecodebox.io,resources=scans/status,verbs=get;update;patch

func (r *ScanReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("scan", req.NamespacedName)

	// get the scan
	var scan scansv1.Scan
	if err := r.Get(ctx, req.NamespacedName, &scan); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		log.V(7).Info("Unable to fetch Scan")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Scan Found", "ScanType", scan.Spec.ScanType)

	// get the scan template for the scan
	var scanTemplate scansv1.ScanTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: scan.Spec.ScanType, Namespace: req.Namespace}, &scanTemplate); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		log.V(7).Info("Unable to fetch ScanTemplate")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Matching ScanTemplate Found", "ScanTemplate", scanTemplate.Name)

	// check if k8s job for scan was already created
	var childJobs batch.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingField(ownerKey, req.Name)); err != nil {
		log.Error(err, "unable to list child Pods")
		return ctrl.Result{}, err
	}

	// TODO: What if the Pod doesn't match our spec? Recreate?

	log.V(9).Info("Got related jobs", "count", len(childJobs.Items))

	if len(childJobs.Items) > 1 {
		// yoo that wasn't expected
		return ctrl.Result{}, errors.New("Scan had more than one job. Thats not expected")
	} else if len(childJobs.Items) == 1 {
		// Job seems to already exist
		log.Info("Job seems already have been created")
		job := childJobs.Items[0]

		scan.Status.Done = job.Status.Succeeded != 0

		if err := r.Status().Update(ctx, &scan); err != nil {
			log.Error(err, "unable to update Scan status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	job, err := r.constructJobForCronJob(&scan, &scanTemplate)
	if err != nil {
		log.Error(err, "unable to create job object ScanTemplate")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, err
	}

	log.V(7).Info("Constructed Job object", "job args", strings.Join(job.Spec.Template.Spec.Containers[0].Args, ", "))

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unable to create Job for Scan", "job", job)
		return ctrl.Result{}, err
	}

	log.V(1).Info("created Job for Scan run", "job", job)

	return ctrl.Result{}, nil
}

func (r *ScanReconciler) constructJobForCronJob(scan *scansv1.Scan, scanTemplate *scansv1.ScanTemplate) (*batch.Job, error) {
	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice

	bucketName := os.Getenv("S3_BUCKET")

	filename := filepath.Base(scanTemplate.Spec.ExtractResults.Location)
	url, err := r.MinioClient.PresignedPutObject(bucketName, fmt.Sprintf("scan-%s/%s", scan.UID, filename), 12*time.Hour)
	if err != nil {
		r.Log.Error(err, "Could not get presigned url from s3 or compatible storage provider")
		return nil, err
	}

	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:       make(map[string]string),
			Annotations:  make(map[string]string),
			GenerateName: fmt.Sprintf("%s-", scan.Name),
			Namespace:    scan.Namespace,
		},
		Spec: *scanTemplate.Spec.JobTemplate.Spec.DeepCopy(),
	}

	job.Spec.Template.Spec.Volumes = []corev1.Volume{
		corev1.Volume{
			Name: "scan-results",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	var containerVolumeMounts []corev1.VolumeMount
	if job.Spec.Template.Spec.Containers[0].VolumeMounts == nil || len(job.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 {
		containerVolumeMounts = []corev1.VolumeMount{}
	} else {
		containerVolumeMounts = job.Spec.Template.Spec.Containers[0].VolumeMounts
	}
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(containerVolumeMounts, []corev1.VolumeMount{corev1.VolumeMount{
		Name:      "scan-results",
		MountPath: "/home/securecodebox/",
	}}...)

	lurcherSidecar := &corev1.Container{
		Name:  "lurcher",
		Image: "docker.pkg.github.com/j12934/securecodebox/lurcher:b943cf1",
		Args: []string{
			"--container",
			job.Spec.Template.Spec.Containers[0].Name,
			"--file",
			scanTemplate.Spec.ExtractResults.Location,
			"--url",
			url.String(),
		},
		Env: []corev1.EnvVar{
			corev1.EnvVar{
				Name: "NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
		},
		// TODO Assign sane default limits for lurcher
		// Resources: corev1.ResourceRequirements{
		// 	Limits: map[corev1.ResourceName]resource.Quantity{
		// 		"": {
		// 			Format: "",
		// 		},
		// 	},
		// 	Requests: map[corev1.ResourceName]resource.Quantity{
		// 		"": {
		// 			Format: "",
		// 		},
		// 	},
		// },
		VolumeMounts: []corev1.VolumeMount{
			corev1.VolumeMount{
				Name:      "scan-results",
				MountPath: "/home/securecodebox/",
			},
		},
		ImagePullPolicy: "IfNotPresent",
	}

	job.Spec.Template.Spec.Containers = append(job.Spec.Template.Spec.Containers, *lurcherSidecar)

	// for k, v := range cronJob.Spec.JobTemplate.Annotations {
	// 	job.Annotations[k] = v
	// }
	// job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	// for k, v := range cronJob.Spec.JobTemplate.Labels {
	// 	job.Labels[k] = v
	// }
	if err := ctrl.SetControllerReference(scan, job, r.Scheme); err != nil {
		return nil, err
	}

	args := append(
		scanTemplate.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command,
		scan.Spec.Parameters...,
	)

	// Using args over commands
	job.Spec.Template.Spec.Containers[0].Args = args
	job.Spec.Template.Spec.Containers[0].Command = nil

	return job, nil
}

func (r *ScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	endpoint := os.Getenv("S3_ENDPOINT")
	accessKeyID := os.Getenv("S3_ACCESS_KEY")
	secretAccessKey := os.Getenv("S3_SECRET_KEY")
	useSSL := true

	// Initialize minio client object.
	minioClient, err := minio.New(endpoint, accessKeyID, secretAccessKey, useSSL)
	if err != nil {
		r.Log.Error(err, "Could not create minio client to communicate with s3 or compatible storage provider")
		panic(err)
	}
	r.MinioClient = *minioClient
	bucketName := os.Getenv("S3_BUCKET")

	bucketExists, err := r.MinioClient.BucketExists(bucketName)
	if err != nil || bucketExists == false {
		r.Log.Error(err, "Could not communicate with s3 or compatible storage provider")
		panic(err)
	}

	// Todo: Better config management

	if err := mgr.GetFieldIndexer().IndexField(&batch.Job{}, ownerKey, func(rawObj runtime.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*batch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a CronJob...
		if owner.APIVersion != apiGVStr || owner.Kind != "Scan" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&scansv1.Scan{}).
		Owns(&batch.Job{}).
		Complete(r)
}