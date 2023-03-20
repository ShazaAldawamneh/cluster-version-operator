package cvo

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	randutil "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-version-operator/lib/resourcebuilder"
	"github.com/openshift/cluster-version-operator/pkg/payload"
	"github.com/openshift/library-go/pkg/verify"
)

func (optr *Operator) defaultPayloadDir() string {
	if len(optr.payloadDir) == 0 {
		return payload.DefaultPayloadDir
	}
	return optr.payloadDir
}

func (optr *Operator) defaultPayloadRetriever() PayloadRetriever {
	return &payloadRetriever{
		kubeClient:           optr.kubeClient,
		operatorName:         optr.name,
		releaseImage:         optr.release.Image,
		namespace:            optr.namespace,
		nodeName:             optr.nodename,
		payloadDir:           optr.defaultPayloadDir(),
		workingDir:           targetUpdatePayloadsDir,
		verifier:             optr.verifier,
		verifyTimeoutOnForce: 2 * time.Minute,
		downloadTimeout:      2 * time.Minute,
	}
}

const (
	targetUpdatePayloadsDir = "/etc/cvo/updatepayloads"
)

type downloadFunc func(context.Context, configv1.Update) (string, error)

type payloadRetriever struct {
	// releaseImage and payloadDir are the default payload identifiers - updates that point
	// to releaseImage will always use the contents of payloadDir
	releaseImage string
	payloadDir   string

	// these fields are used to retrieve the payload when any other payload is specified
	kubeClient   kubernetes.Interface
	workingDir   string
	namespace    string
	nodeName     string
	operatorName string

	// verifier guards against invalid remote data being accessed
	verifier             verify.Interface
	verifyTimeoutOnForce time.Duration

	downloader      downloadFunc
	downloadTimeout time.Duration
}

func (r *payloadRetriever) RetrievePayload(ctx context.Context, update configv1.Update) (PayloadInfo, error) {
	if r.releaseImage == update.Image {
		return PayloadInfo{
			Directory: r.payloadDir,
			Local:     true,
		}, nil
	}

	if len(update.Image) == 0 {
		return PayloadInfo{}, fmt.Errorf("no payload image has been specified and the contents of the payload cannot be retrieved")
	}

	var info PayloadInfo

	// verify the provided payload
	var releaseDigest string
	if index := strings.LastIndex(update.Image, "@"); index != -1 {
		releaseDigest = update.Image[index+1:]
	}
	verifyCtx := ctx

	// if 'force' specified, ensure call to verify payload signature times out well before parent context
	// to allow time to perform forced update
	if update.Force {
		timeout := r.verifyTimeoutOnForce
		if deadline, deadlineSet := ctx.Deadline(); deadlineSet {
			timeout = time.Until(deadline) / 2
		}
		klog.V(2).Infof("Forced update so reducing payload signature verification timeout to %s", timeout)
		var cancel context.CancelFunc
		verifyCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := r.verifier.Verify(verifyCtx, releaseDigest); err != nil {
		vErr := &payload.UpdateError{
			Reason:  "ImageVerificationFailed",
			Message: fmt.Sprintf("The update cannot be verified: %v", err),
			Nested:  err,
		}
		if !update.Force {
			return PayloadInfo{}, vErr
		}
		vErr.Message = fmt.Sprintf("Target release version=%q image=%q cannot be verified, but continuing anyway because the update was forced: %v", update.Version, update.Image, err)
		klog.Warning(vErr)
		info.VerificationError = vErr
	} else {
		info.Verified = true
	}

	if r.downloader == nil {
		r.downloader = r.targetUpdatePayloadDir
	}

	// download the payload to the directory
	var err error
	info.Directory, err = r.downloader(ctx, update)
	if err != nil {
		return PayloadInfo{}, &payload.UpdateError{
			Reason:  "UpdatePayloadRetrievalFailed",
			Message: fmt.Sprintf("Unable to download and prepare the update: %v", err),
		}
	}
	return info, nil
}

func (r *payloadRetriever) targetUpdatePayloadDir(ctx context.Context, update configv1.Update) (string, error) {
	hash := md5.New()
	hash.Write([]byte(update.Image))
	payloadHash := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	tdir := filepath.Join(r.workingDir, payloadHash)

	// Prune older jobs and directories while gracefully handling errors.
	if err := r.pruneJobs(ctx, 0); err != nil {
		klog.Warningf("failed to prune jobs: %v", err)
	}

	if err := payload.ValidateDirectory(tdir); os.IsNotExist(err) {
		// the dirs don't exist, try fetching the payload to tdir.
		if err := r.fetchUpdatePayloadToDir(ctx, tdir, update); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	// now that payload has been loaded check validation.
	if err := payload.ValidateDirectory(tdir); err != nil {
		return "", err
	}
	return tdir, nil
}

func (r *payloadRetriever) fetchUpdatePayloadToDir(ctx context.Context, dir string, update configv1.Update) error {
	var (
		version         = update.Version
		image           = update.Image
		name            = fmt.Sprintf("%s-%s-%s", r.operatorName, version, randutil.String(5))
		namespace       = r.namespace
		deadline        = pointer.Int64Ptr(2 * 60)
		nodeSelectorKey = "node-role.kubernetes.io/master"
		nodename        = r.nodeName
	)

	baseDir, targetName := filepath.Split(dir)
	tmpDir := filepath.Join(baseDir, fmt.Sprintf("%s-%s", targetName, randutil.String(5)))

	setContainerDefaults := func(container corev1.Container) corev1.Container {
		container.Image = image
		container.VolumeMounts = []corev1.VolumeMount{{
			MountPath: targetUpdatePayloadsDir,
			Name:      "payloads",
		}}
		container.SecurityContext = &corev1.SecurityContext{
			Privileged:             pointer.BoolPtr(true),
			ReadOnlyRootFilesystem: pointer.BoolPtr(false),
		}
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("10m"),
				corev1.ResourceMemory:           resource.MustParse("50Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("2Mi"),
			},
		}
		return container
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						setContainerDefaults(corev1.Container{
							Name:       "cleanup",
							Command:    []string{"sh", "-c", "rm -fR ./*"},
							WorkingDir: baseDir,
						}),
						setContainerDefaults(corev1.Container{
							Name:    "make-temporary-directory",
							Command: []string{"mkdir", tmpDir},
						}),
						setContainerDefaults(corev1.Container{
							Name: "move-operator-manifests-to-temporary-directory",
							Command: []string{
								"mv",
								filepath.Join(payload.DefaultPayloadDir, payload.CVOManifestDir),
								filepath.Join(tmpDir, payload.CVOManifestDir),
							},
						}),
						setContainerDefaults(corev1.Container{
							Name: "move-release-manifests-to-temporary-directory",
							Command: []string{
								"mv",
								filepath.Join(payload.DefaultPayloadDir, payload.ReleaseManifestDir),
								filepath.Join(tmpDir, payload.ReleaseManifestDir),
							},
						}),
					},
					Containers: []corev1.Container{
						setContainerDefaults(corev1.Container{
							Name:    "rename-to-final-location",
							Command: []string{"mv", tmpDir, dir},
						}),
					},
					Volumes: []corev1.Volume{{
						Name: "payloads",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: targetUpdatePayloadsDir,
							},
						},
					}},
					NodeName: nodename,
					NodeSelector: map[string]string{
						nodeSelectorKey: "",
					},
					PriorityClassName: "openshift-user-critical",
					Tolerations: []corev1.Toleration{{
						Key: nodeSelectorKey,
					}},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}

	if _, err := r.kubeClient.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	return resourcebuilder.WaitForJobCompletion(ctx, r.kubeClient.BatchV1(), job)
}

// pruneJobs deletes the older, finished jobs in the namespace.
// retain - the number of newest jobs to keep.
func (r *payloadRetriever) pruneJobs(ctx context.Context, retain int) error {
	jobs, err := r.kubeClient.BatchV1().Jobs(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(jobs.Items) <= retain {
		return nil
	}

	// Select jobs to be deleted
	var deleteJobs []batchv1.Job
	for _, job := range jobs.Items {
		switch {
		// Ignore jobs not beginning with operatorName
		case !strings.HasPrefix(job.Name, r.operatorName+"-"):
			break
		default:
			deleteJobs = append(deleteJobs, job)
		}
	}
	if len(deleteJobs) <= retain {
		return nil
	}

	// Sort jobs by StartTime to determine the newest. nil StartTime is assumed newest.
	sort.Slice(deleteJobs, func(i, j int) bool {
		if deleteJobs[i].Status.StartTime == nil {
			return false
		}
		if deleteJobs[j].Status.StartTime == nil {
			return true
		}
		return deleteJobs[i].Status.StartTime.Before(deleteJobs[j].Status.StartTime)
	})

	var errs []error
	for _, job := range deleteJobs[:len(deleteJobs)-retain] {
		err := r.kubeClient.BatchV1().Jobs(r.namespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to delete job %v", job.Name))
		}
	}
	agg := utilerrors.NewAggregate(errs)
	if agg != nil {
		return fmt.Errorf("error deleting jobs: %v", agg.Error())
	}
	return nil
}

// findUpdateFromConfig identifies a desired update from user input or returns false. It will
// resolve payload if the user specifies a version and a matching available update.
func findUpdateFromConfig(config *configv1.ClusterVersion) (configv1.Update, bool) {
	update := config.Spec.DesiredUpdate
	if update == nil {
		return configv1.Update{}, false
	}
	if len(update.Image) == 0 {
		return findUpdateFromConfigVersion(config, update.Version, update.Force)
	}
	return *update, true
}

func findUpdateFromConfigVersion(config *configv1.ClusterVersion, version string, force bool) (configv1.Update, bool) {
	for _, update := range config.Status.AvailableUpdates {
		if update.Version == version && len(update.Image) > 0 {
			return configv1.Update{
				Version: version,
				Image:   update.Image,
				Force:   force,
			}, true
		}
	}
	return configv1.Update{}, false
}
