package ingressController

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
)

// IngressReconciler reconciles an Ingress object
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type IngressAnnotation struct {
	activate                 string // Activate might be used to control whether the ingress is active or not
	domainName               string // DomainName specifies the primary domain
	subDomainName            string // SubDomainName specifies the sub-domain
	region                   string // Region indicates the geographical region of the ingress
	detechChange             bool   // DetectChanges indicates whether to detect changes in the configuration
	lastAppliedConfiguration interface{}
}

//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses/finalizers,verbs=update

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var ingress networkingv1.Ingress
	var ingressAnnotation IngressAnnotation
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		log.Info("Ingress deleted")
		return ctrl.Result{}, nil
	}

	check, err := r.getAnnotations(&ingress, &ingressAnnotation)
	if err != nil && !check {
		log.Info(err.Error())
		return ctrl.Result{}, nil
	}

	// Create a new AWS session
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(ingressAnnotation.region),
	})
	if err != nil {
		log.Info("Failed to create AWS session for region: " + ingressAnnotation.region)
		return ctrl.Result{}, nil
	}

	// Create a new Route 53 client
	r53 := route53.New(sess)

	// Retrieve the hosted zone ID using the domain name
	hostedZoneID, err := r.findHostedZoneID(r53, ingressAnnotation.domainName)
	if err != nil {
		log.Info(err.Error())
		log.Info(fmt.Sprintf("Failed to find hosted zone ID for %s", ingressAnnotation.domainName))
		return ctrl.Result{}, nil
	}

	// Get the LoadBalancer IP from the Ingress
	lbIP, err := r.getLoadBalancerIP(ctx, &ingress)
	if err != nil {
		log.Info("Waiting to get loadBalancer IP")
		return ctrl.Result{}, nil
	}

	if ingressAnnotation.detechChange {
		log.Info("Detected changes in the configuration")
		// Delete the existing DNS record
		recordName := ingressAnnotation.lastAppliedConfiguration.(map[string]interface{})["subdomain-name"].(string) + "." + ingressAnnotation.lastAppliedConfiguration.(map[string]interface{})["domain-name"].(string) + "."
		hostedZoneID := ingressAnnotation.lastAppliedConfiguration.(map[string]interface{})["hosted-zone-id"].(string)
		recordValue := ingressAnnotation.lastAppliedConfiguration.(map[string]interface{})["record-value"].(string)
		log.Info("recordName: " + recordName)
		log.Info("hostedZoneID: " + hostedZoneID)
		if err := r.createOrUpdateDNSRecord(r53, hostedZoneID, recordName, recordValue, "DELETE"); err != nil {
			log.Info(err.Error())
			log.Info("Failed to delete the existing DNS record")
			return ctrl.Result{}, nil
		}
	}

	action := "UPSERT"
	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Ingress is being deleted")
		action = "DELETE"
	}

	recordName := ingressAnnotation.subDomainName + "." + ingressAnnotation.domainName

	// Create or update the DNS record
	if err := r.createOrUpdateDNSRecord(r53, hostedZoneID, recordName, lbIP, action); err != nil {
		log.Info("Failed to manage Route 53 record")
		return ctrl.Result{}, nil
	}

	// Add finalizer to the Ingress
	if ingress.ObjectMeta.DeletionTimestamp.IsZero() && !containsString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer") {
		ingress.ObjectMeta.Finalizers = append(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove finalizer from the Ingress for Deletion
	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		ingress.ObjectMeta.Finalizers = removeString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, nil
		}
		log.Info("DNS Record deleted successfully")
		return ctrl.Result{}, nil
	}

	// Update the last-applied-configuration annotation
	if action == "UPSERT" {
		annotationMap := map[string]interface{}{
			"domain-name":    ingressAnnotation.domainName,
			"subdomain-name": ingressAnnotation.subDomainName,
			"region":         ingressAnnotation.region,
			"hosted-zone-id": hostedZoneID,
			"record-value":   lbIP,
		}
		annotationValue, err := json.Marshal(annotationMap)
		if err != nil {
			panic(err)
		}

		currentAnnotations := ingress.ObjectMeta.Annotations
		if currentAnnotations == nil {
			currentAnnotations = make(map[string]string)
		}

		currentAnnotations["route53pilot/last-applied-configuration"] = string(annotationValue)
		ingress.ObjectMeta.Annotations = currentAnnotations

		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, nil
		}

		log.Info("Successfully created DNS entry")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, nil
}

func (r *IngressReconciler) getAnnotations(ingress *networkingv1.Ingress, ingressAnnotation *IngressAnnotation) (bool, error) {
	log := log.FromContext(context.TODO())

	last_applied_configuration := ingress.Annotations["route53pilot/last-applied-configuration"]
	last_applied_configuration_data := make(map[string]interface{})
	if last_applied_configuration != "" {
		err := json.Unmarshal([]byte(last_applied_configuration), &last_applied_configuration_data)
		if err != nil {
			log.Info("Failed to unmarshal last-applied-configuration")
			ingressAnnotation.lastAppliedConfiguration = nil
		}
		ingressAnnotation.lastAppliedConfiguration = last_applied_configuration_data
	} else {
		ingressAnnotation.lastAppliedConfiguration = nil
	}

	ingressAnnotation.activate = ingress.Annotations["route53.kubernetes.io/activate"]
	ingressAnnotation.domainName = ingress.Annotations["route53.kubernetes.io/domain-name"]
	ingressAnnotation.subDomainName = ingress.Annotations["route53.kubernetes.io/subdomain-name"]
	ingressAnnotation.region = ingress.Annotations["route53.kubernetes.io/region"]

	// Check if the activate annotation is set to true
	activate := ingress.Annotations["route53.kubernetes.io/activate"]
	if activate != "true" {
		return false, fmt.Errorf("Route53 Pilot not activated")
	}

	// Fetch domain name from annotation
	domainName := ingress.Annotations["route53.kubernetes.io/domain-name"]
	if domainName == "" {
		return false, fmt.Errorf("domain name annotation missing")
	}
	if !isValidDNSName("DOMAIN", domainName) {
		return false, fmt.Errorf("invalid domain name")
	}
	// Fetch subdomain from annotation
	subDomainName := ingress.Annotations["route53.kubernetes.io/subdomain-name"]
	if subDomainName == "" {
		return false, fmt.Errorf("subdomain name annotation missing")
	}
	if !isValidDNSName("SUBDOMAIN", subDomainName) {
		return false, fmt.Errorf("invalid subdomain name")
	}
	// Fetch region from annotation or default to us-east-1
	region := ingress.Annotations["route53.kubernetes.io/region"]
	if region == "" {
		region = "us-east-1"
	}

	if ingressAnnotation.lastAppliedConfiguration != nil {
		lastAppliedConfig, ok := ingressAnnotation.lastAppliedConfiguration.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid last-applied-configuration format")
		}
		if lastAppliedConfig["domain-name"] == domainName && lastAppliedConfig["subdomain-name"] == subDomainName && lastAppliedConfig["region"] == region {
			if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
				ingressAnnotation.detechChange = false
				return true, nil
			} else {
				return false, fmt.Errorf("no changes in the configuration")
			}
		} else {
			ingressAnnotation.detechChange = true
		}
	}
	return true, nil
}

func (r *IngressReconciler) findHostedZoneID(r53 *route53.Route53, domainName string) (string, error) {
	input := &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(domainName),
	}
	result, err := r53.ListHostedZonesByName(input)
	if err != nil {
		return "", err
	}
	for _, zone := range result.HostedZones {
		if *zone.Name == domainName+"." {
			return *zone.Id, nil
		}
	}
	return "", fmt.Errorf("no hosted zone found matching domain name: %s", domainName)
}

func (r *IngressReconciler) getLoadBalancerIP(ctx context.Context, ingress *networkingv1.Ingress) (string, error) {
	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		return "", fmt.Errorf("waiting to get LoadBalancer IP for Ingress")
	}
	hostname := ingress.Status.LoadBalancer.Ingress[0].Hostname
	return hostname, nil
}

func (r *IngressReconciler) createOrUpdateDNSRecord(r53 *route53.Route53, zoneID, recordName, recordValue, action string) error {
	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(action),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(recordName),
						Type: aws.String("CNAME"),
						TTL:  aws.Int64(300),
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: aws.String(recordValue),
							},
						},
					},
				},
			},
		},
	}
	_, err := r53.ChangeResourceRecordSets(input)
	return err
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Complete(r)
}

func containsString(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func isValidDNSName(domainType string, name string) bool {
	if len(name) > 255 {
		return false
	}
	// Pattern to match each part of DNS (subdomains + top-level domain)
	if domainType == "SUBDOMAIN" {
		dnsPattern := `^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`
		matched, _ := regexp.MatchString(dnsPattern, name)
		return matched
	}
	dnsPattern := `^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	matched, _ := regexp.MatchString(dnsPattern, name)
	return matched
}
