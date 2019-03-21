/*
Copyright 2019 The Knative Authors
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

package tracing

import (
	"context"
	"fmt"
	"time"

	zipkin "github.com/openzipkin/zipkin-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	csWaiting = "Waiting"
	csRunning = "Running"
	csReady   = "Ready"
)

type containerState string

type containerTrace struct {
	span  zipkin.Span
	state containerState
}

// TracePodStartup creates spans detailing the startup process of the pod specified by namespace and listoptions. If more than one pod is found it fails.
func TracePodStartup(ctx context.Context, stopCh <-chan struct{}, eventCh <-chan watch.Event, tracer *zipkin.Tracer) (zipkin.Span, error) {
	var (
		podCreating    zipkin.Span
		podCreatingCtx context.Context
		podState       containerState
	)
	initContainerSpans := make(map[string]*containerTrace)
	containerSpans := make(map[string]*containerTrace)

	for {
		select {
		case ev := <-eventCh:
			if pod, ok := ev.Object.(*corev1.Pod); ok {
				if podState == "" {
					podCreating, podCreatingCtx = tracer.StartSpanFromContext(ctx, "pod_creating")
					podState = csWaiting
				}

				// Set (init)containerSpans
				for _, csCase := range []struct {
					src           []corev1.ContainerStatus
					dest          map[string]*containerTrace
					initContainer bool
				}{{
					src:           pod.Status.InitContainerStatuses,
					dest:          initContainerSpans,
					initContainer: true,
				}, {
					src:           pod.Status.ContainerStatuses,
					dest:          containerSpans,
					initContainer: false,
				}} {
					for _, cs := range csCase.src {
						ct, ok := csCase.dest[cs.Name]
						if !ok {
							span, _ := tracer.StartSpanFromContext(podCreatingCtx, fmt.Sprintf("container_startup_%s", cs.Name))
							csCase.dest[cs.Name] = &containerTrace{
								span:  span,
								state: csWaiting,
							}
							ct = csCase.dest[cs.Name]
						}
						span := ct.span

						if cs.State.Terminated != nil {
							if !csCase.initContainer {
								zipkin.TagError.Set(span, "terminated")
							}
							span.Finish()
						}

						if cs.State.Running != nil && ct.state != csRunning {
							span.Annotate(time.Now(), "running")
							ct.state = csRunning
						}

						if cs.Ready {
							ct.state = csReady
							span.Finish()
						}
					}
				}

				if pod.Status.Phase == corev1.PodRunning {
					if podCreating != nil && podState == csWaiting {
						podState = csRunning
						podCreating.Annotate(time.Now(), "running")
					}
				}

				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						if podCreating != nil {
							podState = csReady
							podCreating.Finish()
						}
						return podCreating, nil
					}
				}
			}
		case <-stopCh:
			if podCreating != nil {
				zipkin.TagError.Set(podCreating, "tracing stopped")
				podCreating.Finish()
			}
			return podCreating, nil
		}
	}

	return podCreating, nil
}
