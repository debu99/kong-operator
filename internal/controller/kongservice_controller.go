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
	"bytes"
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

	// Fetch the KongService instance
	var kongService kongv1.KongService
	if err := r.Get(ctx, req.NamespacedName, &kongService); err != nil {
		logger.Error(err, "Unable to fetch KongService")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Log the reconciliation
	logger.Info("Reconciling KongService", "name", kongService.Name)

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

	// Check if the service exists in the status
	if kongService.Status.ServiceID == "" {
		// Service doesn't exist, create it
		serviceID, err := r.createService(ctx, kongService.Spec.Name, kongService.Spec.URL)
		if err != nil {
			logger.Error(err, "Failed to create Kong service")
			return ctrl.Result{}, err
		}
		kongService.Status.ServiceID = serviceID
		logger.Info("Successfully created KongService", "name", kongService.Name, "serviceID", serviceID)
	} else {
		// Service exists
		logger.Info("Kong Service already exists", "name", kongService.Name, "serviceID", kongService.Status.ServiceID)
	}

	// Initialize RouteIDs map if it's nil
	if kongService.Status.RouteIDs == nil {
		kongService.Status.RouteIDs = make(map[string]string)
	}

	// Create or update routes
	for _, route := range kongService.Spec.Routes {
		routeName := generateRouteName(kongService.Name, kongService.Spec.Name, route.Path)

		existingRouteID := kongService.Status.RouteIDs[routeName]
		routeID, err := r.createOrUpdateRoute(ctx, kongService.Name, kongService.Spec.Name, kongService.Status.ServiceID, route, existingRouteID)
		if err != nil {
			logger.Error(err, "Failed to create/update Kong route", "name", routeName)
			return ctrl.Result{}, err
		}

		if existingRouteID == "" {
			logger.Info("Successfully created Kong Route", "name", routeName, "routeID", routeID)
		} else if routeID != existingRouteID {
			logger.Info("Successfully updated Kong Route", "name", routeName, "routeID", routeID)
		} else {
			logger.Info("Kong Route is up to date", "name", routeName, "routeID", routeID)
		}

		kongService.Status.RouteIDs[routeName] = routeID
	}

	// Check for routes to delete
	for routeName, routeID := range kongService.Status.RouteIDs {
		if !routeExistsInSpec(routeName, kongService.Spec.Routes) {
			if err := r.deleteRoute(ctx, routeID); err != nil {
				logger.Error(err, "Failed to delete Kong route", "name", routeName, "routeID", routeID)
				return ctrl.Result{}, err
			}
			delete(kongService.Status.RouteIDs, routeName)
			logger.Info("Successfully deleted Kong Route", "name", routeName, "routeID", routeID)
		}
	}

	// Update the status
	if err := r.Status().Update(ctx, &kongService); err != nil {
		logger.Error(err, "Failed to update KongService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func generateRouteName(kongServiceName, serviceName, path string) string {
	cleanPath := strings.Trim(path, "/")
	routeName := fmt.Sprintf("%s-%s-%s", kongServiceName, serviceName, cleanPath)
	return strings.ReplaceAll(routeName, "/", "-")
}

func routeExistsInSpec(routeName string, specRoutes []kongv1.Route) bool {
	for _, route := range specRoutes {
		if generateRouteName("", "", route.Path) == routeName {
			return true
		}
	}
	return false
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

func (r *KongServiceReconciler) createOrUpdateRoute(ctx context.Context, kongServiceName, serviceName, serviceID string, route kongv1.Route, existingRouteID string) (string, error) {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	// Create a unique route name
	routeName := generateRouteName(kongServiceName, serviceName, route.Path)

	// Prepare route data
	routeData := map[string]interface{}{
		"name":    routeName,
		"service": map[string]string{"id": serviceID},
		"paths":   []string{route.Path},
		"tags":    append(route.Tags, kongServiceName, serviceName, "k8s-operator"),
	}

	if len(route.Methods) > 0 {
		routeData["methods"] = route.Methods
	}
	if len(route.Hosts) > 0 {
		routeData["hosts"] = route.Hosts
	}
	if route.StripPath != nil {
		routeData["strip_path"] = *route.StripPath
	}
	if route.PreserveHost != nil {
		routeData["preserve_host"] = *route.PreserveHost
	}
	if len(route.Protocols) > 0 {
		routeData["protocols"] = route.Protocols
	} else {
		routeData["protocols"] = []string{"http", "https"}
	}

	var apiURL string
	var method string
	if existingRouteID == "" {
		// Create new route
		apiURL = fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes", cpUUID)
		method = "POST"
	} else {
		// Update existing route
		apiURL = fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes/%s", cpUUID, existingRouteID)
		method = "PATCH"
	}

	payload, err := json.Marshal(routeData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal route data: %w", err)
	}

	req, err := http.NewRequest(method, apiURL, bytes.NewBuffer(payload))
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		logger.Error(fmt.Errorf("API error"), "Failed to create/update route", "statusCode", resp.StatusCode, "response", string(body))
		return "", fmt.Errorf("failed to create/update route, status code: %d, response: %s", resp.StatusCode, string(body))
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
