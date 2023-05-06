package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	autoscalingClient *autoscaling.AutoScaling
	ec2Client         *ec2.EC2

	setRegion          = os.Getenv("REGION")
	caNamespace        = os.Getenv("CA_NAMESPACE")
	caPriorityExpander = "cluster-autoscaler-priority-expander"
	asgContains        = os.Getenv("ASG_CONTAINS")
	ltContains         = os.Getenv("LT_CONTAINS")
	sleepMinutesEnv    = os.Getenv("SLEEP_MINUTES")
	sleepMinutes       int
	loopSleep          time.Duration
	catchAllEnv        = os.Getenv("CATCH_ALL")
	catchAll           bool
	debugEnv           = os.Getenv("DEBUG")
	debug              bool
	skipCMCreationEnv  = os.Getenv("SKIP_CM_CREATION")
	skipCMCreation     bool
)

func init() {
	// Parse environment variables
	sleepMinutes, _ = strconv.Atoi(sleepMinutesEnv)
	loopSleep = time.Duration(sleepMinutes) * time.Minute
	catchAll, _ = strconv.ParseBool(catchAllEnv)
	debug, _ = strconv.ParseBool(debugEnv)
	skipCMCreation, _ = strconv.ParseBool(skipCMCreationEnv)

	// Initialize AWS clients
	sess := session.Must(session.NewSession())
	autoscalingClient = autoscaling.New(sess, &aws.Config{Region: &setRegion})
	ec2Client = ec2.New(sess, &aws.Config{Region: &setRegion})
}

func main() {
	for {
		fmt.Println("Running CA autoconfig...")
		mainLoop()
		if !debug {
			fmt.Printf("Sleeping for %d minute(s)...\n", sleepMinutes)
			time.Sleep(loopSleep)
		} else {
			fmt.Println("DEBUG mode: exiting...")
			break
		}
	}
}

func mainLoop() {
	caPriorities := make(map[int][]string)

	if debug {
		fmt.Println("DEBUG: mainLoop()")

		if asgContains != "" {
			fmt.Println("DEBUG: ASG_CONTAINS: " + asgContains)
		}

		if ltContains != "" {
			fmt.Println("DEBUG: LT_CONTAINS: " + ltContains)
		}
	}

	for _, asg := range awsSearchEC2ASGByName(asgContains) {
		if debug {
			fmt.Println("considering ASG: " + *asg.AutoScalingGroupName)
		}

		var ltName string
		if asg.LaunchTemplate != nil {
			ltName = *asg.LaunchTemplate.LaunchTemplateName
		} else {
			ltName = *asg.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateName
		}

		if strings.Contains(ltName, ltContains) {
			if debug {
				fmt.Println("retrieving free IPs for LT: " + ltName)
			}
			freeIPs := 0
			for _, subnetID := range strings.Split(*asg.VPCZoneIdentifier, ",") {
				subnet, _ := ec2Client.DescribeSubnets(&ec2.DescribeSubnetsInput{
					SubnetIds: []*string{&subnetID},
				})
				freeIPs += int(*subnet.Subnets[0].AvailableIpAddressCount)
			}

			if _, ok := caPriorities[freeIPs]; !ok {
				caPriorities[freeIPs] = []string{*asg.AutoScalingGroupName}
			} else {
				caPriorities[freeIPs] = append(caPriorities[freeIPs], *asg.AutoScalingGroupName)
			}

			if debug {
				fmt.Printf("%s/%s has %d free IPs\n", *asg.AutoScalingGroupName, ltName, freeIPs)
			}
		}
	}

	// Initialize Kubernetes client
	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		fmt.Printf("Unable to load kube config: %v\n", err)
		return
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("Unable to create Kubernetes client: %v\n", err)
		return
	}

	// Check if configmap exists
	configMapExists := false
	_, err = clientset.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caPriorityExpander, metav1.GetOptions{})
	if err == nil {
		configMapExists = true
	}

	// Save config
	data := make(map[string]string)
	priorities := ""
	keys := make([]int, 0, len(caPriorities))
	for k := range caPriorities {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	for _, key := range keys {
		priorities += fmt.Sprintf("%d:\n", key)
		for _, asg := range caPriorities[key] {
			priorities += fmt.Sprintf("  - %s\n", asg)
		}
	}

	if catchAll {
		priorities += "1:\n"
		priorities += "  - .*\n"
	}

	data["priorities"] = priorities

	if debug {
		fmt.Println(data["priorities"])
	}

	if !configMapExists {
		if skipCMCreation {
			fmt.Printf("Skipping creation of configmap: %s/%s\n", caNamespace, caPriorityExpander)
		} else {
			_, err := clientset.CoreV1().ConfigMaps(caNamespace).Create(context.Background(), &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: caPriorityExpander,
				},
				Data: data,
			}, metav1.CreateOptions{})
			if err != nil {
				fmt.Printf("Error creating configmap: %v\n", err)
			} else {
				fmt.Printf("Created configmap: %s/%s\n", caNamespace, caPriorityExpander)
			}
		}
	} else {
		cm, err := clientset.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caPriorityExpander, metav1.GetOptions{})
		if err != nil {
			fmt.Printf("Error retrieving configmap: %v\n", err)
			return
		}
		cm.Data = data
		_, err = clientset.CoreV1().ConfigMaps(caNamespace).Update(context.Background(), cm, metav1.UpdateOptions{})
		if err != nil {
			fmt.Printf("Error updating configmap: %v\n", err)
			return
		}
		fmt.Printf("Updated configmap: %s/%s\n", caNamespace, caPriorityExpander)
	}

}

func awsSearchEC2ASGByName(name string) []*autoscaling.Group {
	var records []*autoscaling.Group

	err := autoscalingClient.DescribeAutoScalingGroupsPages(&autoscaling.DescribeAutoScalingGroupsInput{},
		func(page *autoscaling.DescribeAutoScalingGroupsOutput, lastPage bool) bool {
			for _, group := range page.AutoScalingGroups {
				if strings.Contains(*group.AutoScalingGroupName, name) {
					records = append(records, group)
				}
			}
			return !lastPage
		})
	if err != nil {
		fmt.Printf("Error searching EC2 ASGs by name: %v\n", err)
	}
	return records
}
