package stub

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/Percona-Lab/percona-server-mongodb-operator/pkg/apis/psmdb/v1alpha1"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	motPkg "github.com/percona/mongodb-orchestration-tools/pkg"
	podk8s "github.com/percona/mongodb-orchestration-tools/pkg/pod/k8s"
	"github.com/sirupsen/logrus"
	mgo "gopkg.in/mgo.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	ErrNoRunningMongodContainers = errors.New("no mongod containers in running state")
	MongoDBTimeout               = 3 * time.Second
)

// getReplsetDialInfo returns a *mgo.Session configured to connect (with auth) to a Pod MongoDB
func getReplsetDialInfo(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec, pods []corev1.Pod, usersSecret *corev1.Secret) *mgo.DialInfo {
	addrs := []string{}
	for _, pod := range pods {
		hostname := podk8s.GetMongoHost(pod.Name, m.Name, replset.Name, m.Namespace)
		addrs = append(addrs, hostname+":"+strconv.Itoa(int(m.Spec.Mongod.Net.Port)))
	}
	return &mgo.DialInfo{
		Addrs:          addrs,
		ReplicaSetName: replset.Name,
		Username:       string(usersSecret.Data[motPkg.EnvMongoDBClusterAdminUser]),
		Password:       string(usersSecret.Data[motPkg.EnvMongoDBClusterAdminPassword]),
		Timeout:        MongoDBTimeout,
		FailFast:       true,
	}
}

// getReplsetStatus returns a ReplsetStatus object for a given replica set
func getReplsetStatus(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec) *v1alpha1.ReplsetStatus {
	for _, rs := range m.Status.Replsets {
		if rs.Name == replset.Name {
			return rs
		}
	}
	status := &v1alpha1.ReplsetStatus{
		Name:    replset.Name,
		Members: []*v1alpha1.ReplsetMemberStatus{},
	}
	m.Status.Replsets = append(m.Status.Replsets, status)
	return status
}

// isReplsetInitialized returns a boolean reflecting if the replica set has been initiated
func isReplsetInitialized(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec, status *v1alpha1.ReplsetStatus, podList *corev1.PodList, usersSecret *corev1.Secret) bool {
	if status.Initialized {
		return true
	}

	// try making a replica set connection to the pods to
	// check if the replset was already initialized
	session, err := mgo.DialWithInfo(getReplsetDialInfo(m, replset, podList.Items, usersSecret))
	if err != nil {
		logrus.Debugf("Cannot connect to mongodb replset %s to check initialization: %v", replset.Name, err)
		return false
	}
	defer session.Close()
	err = session.Ping()
	if err != nil {
		logrus.Debugf("Cannot ping mongodb replset %s to check initialization: %v", replset.Name, err)
		return false
	}

	// if we made it here the replset was already initialized
	status.Initialized = true

	return true
}

// getReplsetMemberStatuses returns a list of ReplsetMemberStatus structs for a given replset
func getReplsetMemberStatuses(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec, pods []corev1.Pod, usersSecret *corev1.Secret) []*v1alpha1.ReplsetMemberStatus {
	members := []*v1alpha1.ReplsetMemberStatus{}
	for _, pod := range pods {
		dialInfo := getReplsetDialInfo(m, replset, []corev1.Pod{pod}, usersSecret)
		dialInfo.Direct = true
		session, err := mgo.DialWithInfo(dialInfo)
		if err != nil {
			logrus.Debugf("Cannot connect to mongodb host %s: %v", dialInfo.Addrs[0], err)
			continue
		}
		defer session.Close()
		session.SetMode(mgo.Eventual, true)

		logrus.Debugf("Updating status for host: %s", dialInfo.Addrs[0])

		buildInfo, err := session.BuildInfo()
		if err != nil {
			logrus.Debugf("Cannot get buildInfo from mongodb host %s: %v", dialInfo.Addrs[0], err)
			continue
		}

		members = append(members, &v1alpha1.ReplsetMemberStatus{
			Name:    dialInfo.Addrs[0],
			Version: buildInfo.Version,
		})
	}
	return members
}

// handleReplsetInit runs the k8s-mongodb-initiator from within the first running pod's mongod container.
// This must be ran from within the running container to utilise the MongoDB Localhost Exeception.
//
// See: https://docs.mongodb.com/manual/core/security-users/#localhost-exception
//
func (h *Handler) handleReplsetInit(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec, pods []corev1.Pod) error {
	for _, pod := range pods {
		if !isMongodPod(pod) || !isContainerAndPodRunning(pod, mongodContainerName) || !isPodReady(pod) {
			continue
		}

		logrus.Infof("Initiating replset %s on running pod: %s", replset.Name, pod.Name)

		return execCommandInContainer(pod, mongodContainerName, []string{
			"k8s-mongodb-initiator",
			"init",
		})
	}
	return ErrNoRunningMongodContainers
}

func (h *Handler) handleStatefulSetUpdate(m *v1alpha1.PerconaServerMongoDB, set *appsv1.StatefulSet, replset *v1alpha1.ReplsetSpec, resources *corev1.ResourceRequirements) error {
	var doUpdate bool

	// Ensure the stateful set size is the same as the spec
	err := h.client.Get(set)
	if err != nil {
		return fmt.Errorf("failed to get stateful set for replset %s: %v", replset.Name, err)
	}
	if *set.Spec.Replicas != replset.Size {
		logrus.Infof("setting replicas to %d for replset: %s", replset.Size, replset.Name)
		set.Spec.Replicas = &replset.Size
		doUpdate = true
	}

	// Ensure the PSMDB version is the same as the spec
	expectedImage := getPSMDBDockerImageName(m)
	mongod := &set.Spec.Template.Spec.Containers[0]
	if mongod != nil && mongod.Image != expectedImage {
		logrus.Infof("updating spec image for replset %s: %s -> %s", replset.Name, mongod.Image, expectedImage)
		mongod.Image = expectedImage
		doUpdate = true
	}

	// Ensure the stateful set resources are the same as the spec
	mongodLimits := corev1.ResourceList{
		corev1.ResourceStorage: mongod.Resources.Limits[corev1.ResourceStorage],
	}
	mongodRequests := corev1.ResourceList{}
	for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		mongodRequest := mongod.Resources.Requests[resourceName]
		specRequest := resources.Requests[resourceName]
		if specRequest.Cmp(mongod.Resources.Requests[resourceName]) != 0 {
			logrus.Infof("updating %s resource request: %s -> %s", resourceName, mongodRequest.String(), specRequest.String())
			mongodRequests[resourceName] = specRequest
			doUpdate = true
		} else {
			mongodRequests[resourceName] = mongodRequest
		}

		mongodLimit := mongod.Resources.Limits[resourceName]
		specLimit := resources.Limits[resourceName]
		if specLimit.Cmp(mongodLimit) != 0 && specLimit.Cmp(mongodRequest) >= 0 {
			logrus.Infof("updating %s resource limit: %s -> %s", resourceName, mongodLimit.String(), specLimit.String())
			mongodLimits[resourceName] = specLimit
			doUpdate = true
		} else {
			mongodLimits[resourceName] = mongodLimit
		}
	}
	mongod.Resources.Limits = mongodLimits
	mongod.Resources.Requests = mongodRequests

	// Ensure mongod args are the same as the args from the spec:
	expectedMongodArgs := newPSMDBMongodContainerArgs(m, replset, resources)
	if !reflect.DeepEqual(expectedMongodArgs, mongod.Args) {
		logrus.Infof("updating container mongod args for replset %s: %v -> %v", replset.Name, mongod.Args, expectedMongodArgs)
		mongod.Args = expectedMongodArgs
		doUpdate = true
	}

	// Do update if something changed
	if doUpdate {
		logrus.Infof("updating state for replset: %s", replset.Name)
		err = h.client.Update(set)
		if err != nil {
			return fmt.Errorf("failed to update stateful set for replset %s: %v", replset.Name, err)
		}
	}

	return nil
}

// updateStatus updates the PerconaServerMongoDB status
func (h *Handler) updateStatus(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec, usersSecret *corev1.Secret) (*corev1.PodList, error) {
	var doUpdate bool

	podList := podList()
	err := h.client.List(m.Namespace, podList, sdk.WithListOptions(getLabelSelectorListOpts(m, replset)))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for replset %s: %v", replset.Name, err)
	}

	// Update status pods list
	podNames := getPodNames(podList.Items)
	status := getReplsetStatus(m, replset)
	if !reflect.DeepEqual(podNames, status.Pods) {
		status.Pods = podNames
		doUpdate = true
	}

	// Update mongodb replset member status list
	members := getReplsetMemberStatuses(m, replset, podList.Items, usersSecret)
	if !reflect.DeepEqual(members, status.Members) {
		status.Members = members
		doUpdate = true
	}

	// Send update to SDK if something changed
	if doUpdate {
		err = h.client.Update(m)
		if err != nil {
			return nil, fmt.Errorf("failed to update status for replset %s: %v", replset.Name, err)
		}
	}

	// Update the pods list that is read by the watchdog
	if h.pods == nil {
		h.pods = podk8s.NewPods(m.Name, m.Namespace)
	}
	h.pods.SetPods(podList.Items)

	return podList, nil
}

// ensureReplsetStatefulSet ensures a StatefulSet exists
func (h *Handler) ensureReplsetStatefulSet(m *v1alpha1.PerconaServerMongoDB, replset *v1alpha1.ReplsetSpec) error {
	limits, err := parseSpecResourceRequirements(replset.Limits)
	if err != nil {
		return err
	}
	requests, err := parseSpecResourceRequirements(replset.Requests)
	if err != nil {
		return err
	}
	resources := corev1.ResourceRequirements{
		Limits:   limits,
		Requests: requests,
	}

	lf := logrus.Fields{
		"version": m.Spec.Version,
		"size":    replset.Size,
		"cpu":     replset.Limits.Cpu,
		"memory":  replset.Limits.Memory,
		"storage": replset.Limits.Storage,
	}
	if replset.StorageClass != "" {
		lf["storageClass"] = replset.StorageClass
	}

	set, err := h.newPSMDBStatefulSet(m, replset, &resources)
	if err != nil {
		return err
	}
	err = h.client.Create(set)
	if err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return err
		}
	} else {
		logrus.WithFields(lf).Infof("created stateful set for replset: %s", replset.Name)
	}

	// Ensure the spec is up to date
	err = h.handleStatefulSetUpdate(m, set, replset, &resources)
	if err != nil {
		return fmt.Errorf("failed to get stateful set for replset %s: %v", replset.Name, err)
	}

	return nil
}

// ensureReplset ensures resources for a PSMDB replset exist
func (h *Handler) ensureReplset(m *v1alpha1.PerconaServerMongoDB, podList *corev1.PodList, replset *v1alpha1.ReplsetSpec, usersSecret *corev1.Secret) (*v1alpha1.ReplsetStatus, error) {
	// Create the StatefulSet if it doesn't exist
	err := h.ensureReplsetStatefulSet(m, replset)
	if err != nil {
		logrus.Errorf("failed to create stateful set for replset %s: %v", replset.Name, err)
		return nil, err
	}

	// Initiate the replset if it hasn't already been initiated + there are pods +
	// we have waited the ReplsetInitWait period since starting
	status := getReplsetStatus(m, replset)
	if !isReplsetInitialized(m, replset, status, podList, usersSecret) && len(podList.Items) >= 1 && time.Since(h.startedAt) > ReplsetInitWait {
		err = h.handleReplsetInit(m, replset, podList.Items)
		if err != nil {
			return nil, err
		}

		// update status after replset init
		status.Initialized = true
		err = h.client.Update(m)
		if err != nil {
			return nil, fmt.Errorf("failed to update status for replset %s: %v", replset.Name, err)
		}
		logrus.Infof("changed state to initialised for replset %s", replset.Name)

		// ensure the watchdog is started
		err = h.ensureWatchdog(m, usersSecret)
		if err != nil {
			return nil, fmt.Errorf("failed to start watchdog: %v", err)
		}
	}

	// Create service for replset
	service := newPSMDBService(m, replset)
	err = h.client.Create(service)
	if err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			logrus.Errorf("failed to create psmdb service: %v", err)
			return nil, err
		}
	} else {
		logrus.Infof("created service %s", service.Name)
	}

	return status, nil
}
