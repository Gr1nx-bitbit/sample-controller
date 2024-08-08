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

package main

import (
	// "container/list"
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	apisv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	lists "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	clientset "github.com/Gr1nx-bitbit/pod-customizer/pkg/generated/clientset/versioned"
	samplescheme "github.com/Gr1nx-bitbit/pod-customizer/pkg/generated/clientset/versioned/scheme"
	informers "github.com/Gr1nx-bitbit/pod-customizer/pkg/generated/informers/externalversions/samplecontroller/v1alpha1"
	listers "github.com/Gr1nx-bitbit/pod-customizer/pkg/generated/listers/samplecontroller/v1alpha1"
)

const controllerAgentName = "sample-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Foo synced successfully"
	// FieldManager distinguishes this controller from other things writing to API objects
	FieldManager = controllerAgentName
)

// Controller is the controller implementation for Foo resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// sampleclientset is a clientset for our own API group
	sampleclientset clientset.Interface

	podCustomizerList    listers.PodCustomizerLister
	podCustomizersSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.TypedRateLimitingInterface[cache.ObjectName]
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	sampleclientset clientset.Interface,
	podCustomizerInformer informers.PodCustomizerInformer) *Controller {
	logger := klog.FromContext(ctx)

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	utilruntime.Must(samplescheme.AddToScheme(scheme.Scheme))
	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	ratelimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[cache.ObjectName](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[cache.ObjectName]{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	controller := &Controller{
		kubeclientset:        kubeclientset,
		sampleclientset:      sampleclientset,
		podCustomizerList:    podCustomizerInformer.Lister(),
		podCustomizersSynced: podCustomizerInformer.Informer().HasSynced,
		workqueue:            workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:             recorder,
	}

	logger.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	podCustomizerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueuePodCustomizer,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueuePodCustomizer(new)
		},
		DeleteFunc: func(obj interface{}) {
			if name, err := cache.ObjectToName(obj); err != nil {
				panic(err)
			} else {
				klog.InfoS("delete callback invoked, just letting you know", "key", name)
			}
		},
	})

	// set up an event handler for when Pod resources change. Have the handler
	// update a PodCustomizer's status to inspect the pod and either promote or
	// destroy it
	var podList lists.PodInformer
	podList.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.updateCustomizer,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.updateCustomizer(newObj)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)

	// Start the informer factories to begin populating the informer caches
	logger.Info("Starting PodCustomizer controller")

	// Wait for the caches to be synced before starting workers
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.podCustomizersSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count" /*workers*/, 1)

	// Launch two workers to process Foo resources
	// for i := 0; i < workers; i++ {
	go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	// }

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

func (c *Controller) updateCustomizer(obj interface{}) {
	// whenever a pod is added, I want to check for its ownerReference | actually I'll do this is in the business logic
	// and then update a CR with its name and namespace to be deleted or promoted
	objectRef, err := cache.ObjectToName(obj) // reference to the pod
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// list of the CRs in the default namespace
	customs, err := c.sampleclientset.SamplecontrollerV1alpha1().PodCustomizers("default").List(context.TODO(), v1.ListOptions{})
	if err != nil {
		klog.Info("error retrieving list of podCustomizers")
		return
	}

	for _, item := range customs.Items {
		custom := item.DeepCopy()
		custom.Status.TargetPod = objectRef.Name
		custom.Status.TargetNamespace = objectRef.Namespace
		c.sampleclientset.SamplecontrollerV1alpha1().PodCustomizers("default").Update(context.TODO(), custom, v1.UpdateOptions{})
		break
	}

}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	objRef, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	// We call Done at the end of this func so the workqueue knows we have
	// finished processing this item. We also must remember to call Forget
	// if we do not want this work item being re-queued. For example, we do
	// not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off
	// period.
	defer c.workqueue.Done(objRef)

	// Run the syncHandler, passing it the structured reference to the object to be synced.
	err := c.syncHandler(ctx, objRef)
	if err == nil {
		// If no error occurs then we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(objRef)
		logger.Info("Successfully synced", "objectName", objRef)
		return true
	}
	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// cluster-monitoring.
	utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
	// since we failed, we should requeue the item to work on later.  This
	// method will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	c.workqueue.AddRateLimited(objRef)
	return true
}

func intToPointer(num int) *int32 {
	n := int32(num)
	return &n
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)
	klog.Info("controller is running!")

	// first, I want to check if the target pod and target namespace fields are there
	// get the CR
	customizer, err := c.podCustomizerList.PodCustomizers(objectRef.Namespace).Get(objectRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, err, "Customizer referenced by item in work queue no longer exists", "objectReference", objectRef)
			return nil
		}

		return err
	}

	targetPod, err := c.kubeclientset.CoreV1().Pods(customizer.Status.TargetNamespace).Get(context.TODO(), customizer.Status.TargetPod, v1.GetOptions{})
	if err != nil {
		return err
	}

	// just realized I need to put an originator namespace, unless I want to just promote the pod in the same namespace
	if customizer.Status.TargetPod != "" && customizer.Status.TargetNamespace != "" && customizer.Spec.Promote && len(targetPod.GetOwnerReferences()) == 0 {

		podSpec := corev1.PodTemplateSpec{
			ObjectMeta: v1.ObjectMeta{
				Name:   targetPod.Name,
				Labels: targetPod.Labels,
			},
			Spec: targetPod.Spec,
		}

		logger.Info("Creating pod promotion deployment")

		deployment := &apisv1.Deployment{
			ObjectMeta: v1.ObjectMeta{
				Name:      targetPod.Name + "-deployment",
				Namespace: customizer.Status.TargetNamespace,
			},
			Spec: apisv1.DeploymentSpec{
				Replicas: intToPointer(2),
				Selector: &v1.LabelSelector{
					MatchLabels: targetPod.Labels,
				},
				Template: podSpec,
			},
		}

		logger.Info("Applying pod promotion as deployment")

		_, err = c.kubeclientset.AppsV1().Deployments(customizer.Status.TargetNamespace).Create(context.TODO(), deployment, v1.CreateOptions{})
		if err != nil {
			return err
		}

		logger.Info("Deleting promoted pod's originator pod")

		err = c.kubeclientset.CoreV1().Pods(customizer.Status.TargetNamespace).Delete(context.TODO(), targetPod.Name, v1.DeleteOptions{})
		if err != nil {
			return err
		}

		update := customizer.DeepCopy()
		update.Status.NumPromoted += 1

		logger.Info("updating podCustimzer numPromoted", objectRef.Name, objectRef.Namespace)
		_, err = c.sampleclientset.SamplecontrollerV1alpha1().PodCustomizers(objectRef.Namespace).Update(context.TODO(), update, v1.UpdateOptions{})
		if err != nil {
			return err
		}

		return nil

	} else if customizer.Status.TargetPod != "" && customizer.Status.TargetNamespace != "" && !customizer.Spec.Promote && len(targetPod.GetOwnerReferences()) == 0 {
		logger.Info("deleting pod", customizer.Status.TargetPod, customizer.Status.TargetNamespace)
		err := c.kubeclientset.CoreV1().Pods(customizer.Status.TargetNamespace).Delete(context.TODO(), customizer.Status.TargetPod, v1.DeleteOptions{})
		if err != nil {
			return err
		}

		logger.Info("updating podCustomizer numDeleted", objectRef.Name, objectRef.Namespace)
		update := customizer.DeepCopy()
		update.Status.NumDeleted += 1
		_, err = c.sampleclientset.SamplecontrollerV1alpha1().PodCustomizers(customizer.Status.TargetNamespace).Update(context.TODO(), update, v1.UpdateOptions{})
		if err != nil {
			return err
		}

		return nil
	}

	logger.Info("no work to do")
	return nil
}

// enqueueFoo takes a Foo resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Foo.
func (c *Controller) enqueuePodCustomizer(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		klog.InfoS("adding to queue", "key", objectRef)
		c.workqueue.Add(objectRef)
	}
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the Foo resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that Foo resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.

// func (c *Controller) handleObject(obj interface{}) {
// 	var object metav1.Object
// 	var ok bool
// 	logger := klog.FromContext(context.Background())
// 	if object, ok = obj.(metav1.Object); !ok {
// 		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
// 		if !ok {
// 			// If the object value is not too big and does not contain sensitive information then
// 			// it may be useful to include it.
// 			utilruntime.HandleErrorWithContext(context.Background(), nil, "Error decoding object, invalid type", "type", fmt.Sprintf("%T", obj))
// 			return
// 		}
// 		object, ok = tombstone.Obj.(metav1.Object)
// 		if !ok {
// 			// If the object value is not too big and does not contain sensitive information then
// 			// it may be useful to include it.
// 			utilruntime.HandleErrorWithContext(context.Background(), nil, "Error decoding object tombstone, invalid type", "type", fmt.Sprintf("%T", tombstone.Obj))
// 			return
// 		}
// 		logger.V(4).Info("Recovered deleted object", "resourceName", object.GetName())
// 	}
// 	logger.V(4).Info("Processing object", "object", klog.KObj(object))
// 	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
// 		// If this object is not owned by a Foo, we should not do anything more
// 		// with it.
// 		if ownerRef.Kind != "Foo" {
// 			return
// 		}

// 		foo, err := c.foosLister.Foos(object.GetNamespace()).Get(ownerRef.Name)
// 		if err != nil {
// 			logger.V(4).Info("Ignore orphaned object", "object", klog.KObj(object), "foo", ownerRef.Name)
// 			return
// 		}

// 		c.enqueueFoo(foo)
// 		return
// 	}
// }

// newDeployment creates a new Deployment for a Foo resource. It also sets
// the appropriate OwnerReferences on the resource so handleObject can discover
// the Foo resource that 'owns' it.

// func newDeployment(foo *samplev1alpha1.PodCustomizer) *appsv1.Deployment {
// 	labels := map[string]string{
// 		"app":        "nginx",
// 		"controller": foo.Name,
// 	}
// 	return &appsv1.Deployment{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      foo.Spec.DeploymentName,
// 			Namespace: foo.Namespace,
// 			OwnerReferences: []metav1.OwnerReference{
// 				*metav1.NewControllerRef(foo, samplev1alpha1.SchemeGroupVersion.WithKind("Foo")),
// 			},
// 		},
// 		Spec: appsv1.DeploymentSpec{
// 			Replicas: foo.Spec.Replicas,
// 			Selector: &metav1.LabelSelector{
// 				MatchLabels: labels,
// 			},
// 			Template: corev1.PodTemplateSpec{
// 				ObjectMeta: metav1.ObjectMeta{
// 					Labels: labels,
// 				},
// 				Spec: corev1.PodSpec{
// 					Containers: []corev1.Container{
// 						{
// 							Name:  "nginx",
// 							Image: "nginx:latest",
// 						},
// 					},
// 				},
// 			},
// 		},
// 	}
// }
