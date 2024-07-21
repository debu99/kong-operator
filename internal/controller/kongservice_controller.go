/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kongv1 "github.com/debu99/kong-operator/api/v1"
)

const kongFinalizerName = "kong.example.com/finalizer"

// KongServiceReconciler reconciles a KongService object
type KongServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KongService object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *KongServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Log the reconciliation
	logger.Info("Reconciling KongService", "name", kongService.Name)

	// Fetch the KongService instance
	var kongService kongv1.KongService
	if err := r.Get(ctx, req.NamespacedName, &kongService); err != nil {
		logger.Error(err, "Unable to fetch KongService")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the KongService instance is marked to be deleted
	if !kongService.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &kongService)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(&kongService, kongFinalizerName) {
		controllerutil.AddFinalizer(&kongService, kongFinalizerName)
		if err := r.Update(ctx, &kongService); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if the service already exists
	if kongService.Status.ServiceID == "" {
		// Create the service
		serviceID, err := r.createService(ctx, kongService.Spec.Name, kongService.Spec.URL)
		if err != nil {
			logger.Error(err, "Failed to create Kong service")
			return ctrl.Result{}, err
		}

		// Update the status
		kongService.Status.ServiceID = serviceID
		if err := r.Status().Update(ctx, &kongService); err != nil {
			logger.Error(err, "Failed to update KongService status")
			return ctrl.Result{}, err
		}

		logger.Info("Successfully created KongService", "name", kongService.Name, "serviceID", serviceID)
	}

	// Initialize RouteIDs map if it's nil
	if kongService.Status.RouteIDs == nil {
		kongService.Status.RouteIDs = make(map[string]string)
	}

	// Create or update routes
	for _, route := range kongService.Spec.Routes {
		if routeID, exists := kongService.Status.RouteIDs[route.Path]; !exists {
			// Create new route
			newRouteID, err := r.createRoute(ctx, kongService.Status.ServiceID, route.Path)
			if err != nil {
				logger.Error(err, "Failed to create Kong route", "path", route.Path)
				return ctrl.Result{}, err
			}
			kongService.Status.RouteIDs[route.Path] = newRouteID
			logger.Info("Successfully created Kong Route", "path", route.Path, "routeID", newRouteID)
		} else {
			// Route exists, you might want to update it if necessary
			// For now, we'll just log that it exists
			logger.Info("Kong Route already exists", "path", route.Path, "routeID", routeID)
		}
	}

	// Check for routes to delete
	for path, routeID := range kongService.Status.RouteIDs {
		routeExists := false
		for _, route := range kongService.Spec.Routes {
			if route.Path == path {
				routeExists = true
				break
			}
		}
		if !routeExists {
			// Delete the route
			if err := r.deleteRoute(ctx, routeID); err != nil {
				logger.Error(err, "Failed to delete Kong route", "path", path, "routeID", routeID)
				return ctrl.Result{}, err
			}
			delete(kongService.Status.RouteIDs, path)
			logger.Info("Successfully deleted Kong Route", "path", path, "routeID", routeID)
		}
	}

	// Update the status
	if err := r.Status().Update(ctx, &kongService); err != nil {
		logger.Error(err, "Failed to update KongService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KongServiceReconciler) reconcileDelete(ctx context.Context, kongService *kongv1.KongService) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Delete all routes
	for path, routeID := range kongService.Status.RouteIDs {
		if err := r.deleteRoute(ctx, routeID); err != nil {
			logger.Error(err, "Failed to delete Kong route", "path", path, "routeID", routeID)
			return ctrl.Result{}, err
		}
		logger.Info("Successfully deleted Kong Route", "path", path, "routeID", routeID)
	}

	// Delete the Kong service
	if kongService.Status.ServiceID != "" {
		if err := r.deleteService(ctx, kongService.Status.ServiceID); err != nil {
			logger.Error(err, "Failed to delete Kong service")
			return ctrl.Result{}, err
		}
		logger.Info("Successfully deleted Kong Service", "serviceID", kongService.Status.ServiceID)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(kongService, kongFinalizerName)
	if err := r.Update(ctx, kongService); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KongServiceReconciler) createService(ctx context.Context, name, url string) (string, error) {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/services", cpUUID)
	payload := strings.NewReader(fmt.Sprintf(`{"name":"%s","url":"%s"}`, name, url))

	req, err := http.NewRequest("POST", apiURL, payload)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		logger.Error(fmt.Errorf("API error"), "Failed to create service", "statusCode", resp.StatusCode, "response", string(body))
		return "", fmt.Errorf("failed to create service, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %w", err)
	}

	serviceID, ok := result["id"].(string)
	if !ok {
		return "", fmt.Errorf("service ID not found in response")
	}

	return serviceID, nil
}

func (r *KongServiceReconciler) createRoute(ctx context.Context, serviceID, path string) (string, error) {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes", cpUUID)
	payload := strings.NewReader(fmt.Sprintf(`{"service":{"id":"%s"},"paths":["%s"]}`, serviceID, path))

	req, err := http.NewRequest("POST", apiURL, payload)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		logger.Error(fmt.Errorf("API error"), "Failed to create route", "statusCode", resp.StatusCode, "response", string(body))
		return "", fmt.Errorf("failed to create route, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %w", err)
	}

	routeID, ok := result["id"].(string)
	if !ok {
		return "", fmt.Errorf("route ID not found in response")
	}

	return routeID, nil
}

func (r *KongServiceReconciler) deleteService(ctx context.Context, serviceID string) error {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/services/%s", cpUUID, serviceID)

	req, err := http.NewRequest("DELETE", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := ioutil.ReadAll(resp.Body)
		logger.Error(fmt.Errorf("API error"), "Failed to delete service", "statusCode", resp.StatusCode, "response", string(body))
		return fmt.Errorf("failed to delete service, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (r *KongServiceReconciler) deleteRoute(ctx context.Context, routeID string) error {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes/%s", cpUUID, routeID)

	req, err := http.NewRequest("DELETE", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := ioutil.ReadAll(resp.Body)
		logger.Error(fmt.Errorf("API error"), "Failed to delete route", "statusCode", resp.StatusCode, "response", string(body))
		return fmt.Errorf("failed to delete route, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KongServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kongv1.KongService{}).
		Complete(r)
}
