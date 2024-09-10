package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var clientset *kubernetes.Clientset

func main() {
	// Determine kubeconfig file path based on OS
	var kubeconfig string
	if runtime.GOOS == "windows" {
		// Windows system
		kubeconfig = filepath.Join(os.Getenv("USERPROFILE"), ".kube", "config")
	} else {
		// Unix-based systems (Linux, macOS)
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	// Load kubeconfig file
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(fmt.Sprintf("Error loading kubeconfig file: %v", err))
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("Error creating Kubernetes client: %v", err))
	}

	// Initialize Gin router
	r := gin.Default()

	// Define the route for streaming logs
	r.GET("/logs", streamPodLogs)

	// Run the API server on port 8080
	r.Run(":8080")
}

// streamPodLogs handles the request to stream logs from a pod's containers, including init containers
func streamPodLogs(c *gin.Context) {
	namespace := c.Query("namespace")
	podName := c.Query("podName")

	if namespace == "" || podName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace and podName are required"})
		return
	}

	// Get the pod details
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Error getting pod: %v", err)})
		return
	}

	// Buffer to collect log output from all containers
	var finalBuffer bytes.Buffer
	var wg sync.WaitGroup
	mu := &sync.Mutex{} // Protect shared buffer access

	// List of all containers (regular + init containers)
	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)

	// Stream logs for each container (regular and init) concurrently
	for _, container := range containers {
		wg.Add(1)
		go func(containerName string) {
			defer wg.Done()

			// Each container will have its own buffer for log aggregation
			var containerBuffer bytes.Buffer

			err := streamLogsFromContainer(&containerBuffer, namespace, podName, containerName)
			if err != nil {
				containerBuffer.WriteString(fmt.Sprintf("Error streaming logs for container %s: %v\n", containerName, err))
			}

			// Append container logs to the final buffer (protected by mutex)
			mu.Lock()
			finalBuffer.WriteString(fmt.Sprintf("Logs for container: %s\n", containerName))
			finalBuffer.Write(containerBuffer.Bytes())
			finalBuffer.WriteString("\n--- End of logs for container: " + containerName + " ---\n\n")
			mu.Unlock()

		}(container.Name)
	}

	wg.Wait()

	// Return the aggregated logs as response
	c.Data(http.StatusOK, "text/plain", finalBuffer.Bytes())
}

// streamLogsFromContainer handles streaming logs for a specific container (including init containers)
func streamLogsFromContainer(buffer *bytes.Buffer, namespace, podName, containerName string) error {
	timeout := 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout reached while waiting for container %s", containerName)
		default:
			// Check the status of the container
			pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("error getting pod: %v", err)
			}

			containerStatus := getContainerStatus(pod, containerName)
			if containerStatus == nil {
				buffer.WriteString(fmt.Sprintf("No status found for container %s\n", containerName))
				return nil
			}

			// Stream logs if the container is running or has terminated
			if containerStatus.State.Running != nil || containerStatus.State.Terminated != nil {
				err = streamLogs(ctx, buffer, namespace, podName, containerName)
				if err != nil {
					return err
				}
				return nil
			}

			// Wait a bit before checking again
			time.Sleep(2 * time.Second)
		}
	}
}

// streamLogs streams the logs of a single container and writes to the provided buffer
func streamLogs(ctx context.Context, buffer *bytes.Buffer, namespace, podName, containerName string) error {
	logOptions := &v1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}

	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, logOptions)
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("error opening stream for container %s: %v", containerName, err)
	}
	defer stream.Close()

	_, err = io.Copy(buffer, stream)
	if err != nil {
		return fmt.Errorf("error copying log stream for container %s: %v", containerName, err)
	}
	return nil
}

// getContainerStatus returns the status of the container with the given name in the pod
func getContainerStatus(pod *v1.Pod, containerName string) *v1.ContainerStatus {
	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if status.Name == containerName {
			return &status
		}
	}
	return nil
}
