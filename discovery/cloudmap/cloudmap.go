// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudmap

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"net"
	"strings"

	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/servicediscovery"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/discovery/refresh"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

type AccountDetails struct {
	Service     string
	Environment string
}

var AccountsDetails = map[string]AccountDetails{
	"685154231839": {Service: "marketing", Environment: "staging"},
	"973393464270": {Service: "marketing", Environment: "production"},
	"882639863719": {Service: "next", Environment: "edge"},
	"385945872227": {Service: "next", Environment: "staging"},
	"421997533442": {Service: "next", Environment: "production"},
	"446227850179": {Service: "nextApps", Environment: "edge"},
	"915340941395": {Service: "nextApps", Environment: "staging"},
	"222265345133": {Service: "nextApps", Environment: "production"},
	"927202343420": {Service: "studio", Environment: "edge"},
	"178132926059": {Service: "studio", Environment: "staging"},
	"664233159730": {Service: "studio", Environment: "production"},
	"765774387714": {Service: "studioApps", Environment: "edge"},
	"703938180312": {Service: "studioApps", Environment: "staging"},
	"788992439060": {Service: "studioApps", Environment: "production"},
	"015600971885": {Service: "screens", Environment: "edge"},
	"649318574937": {Service: "screens", Environment: "staging"},
	"237831806633": {Service: "screens", Environment: "production"},
	"311110039411": {Service: "root", Environment: "production"},
}

const (
	cloudMapLabel                     = "" //model.MetaLabelPrefix + "cloudMap_"
	cloudMapLabelAZ                   = cloudMapLabel + "availability_zone"
	cloudMapLabelInstanceID           = cloudMapLabel + "instance_id"
	cloudMapLabelInstanceState        = cloudMapLabel + "instance_state"
	cloudMapLabelClusterName          = cloudMapLabel + "cluster_name"
	cloudMapLabelPrivateIP            = cloudMapLabel + "private_ip"
	cloudMapLabelTaskDefinitionFamily = cloudMapLabel + "ecs_task_definition_family"
	cloudMapLabelServiceName          = cloudMapLabel + "service_name"
	cloudMapLabelAccountId            = cloudMapLabel + "account_id"
	cloudMapLabelAccountService       = cloudMapLabel + "account_service"
	cloudMapLabelAccountEnvironment   = cloudMapLabel + "account_environment"
)

// DefaultSDConfig is the default EC2 SD configuration.
var DefaultSDConfig = SDConfig{
	Port:            5000,
	RefreshInterval: model.Duration(60 * time.Second),
}

// SDConfig is the configuration for EC2 based service discovery.
type SDConfig struct {
	RoleARN         string         `yaml:"role_arn,omitempty"`
	RefreshInterval model.Duration `yaml:"refresh_interval,omitempty"`
	Port            int            `yaml:"port"`
	Accounts        []string       `yaml:"accounts,omitempty"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	return nil
}

// Discovery periodically performs Cloud Map requests. It implements
// the Discoverer interface.
type Discovery struct {
	*refresh.Discovery
	interval time.Duration
	roleARN  string
	port     int
	logger   log.Logger
}

func NewDiscovery(conf *SDConfig, logger log.Logger) *Discovery {

	if logger == nil {
		logger = log.NewNopLogger()
	}

	d := &Discovery{
		roleARN:  conf.RoleARN,
		interval: time.Duration(conf.RefreshInterval),
		port:     conf.Port,
		logger:   logger,
	}

	d.Discovery = refresh.NewDiscovery(
		logger,
		"cloudmap",
		time.Duration(conf.RefreshInterval),
		d.refresh,
	)

	return d
}

func (d *Discovery) refresh(ctx context.Context) ([]*targetgroup.Group, error) {

	level.Debug(d.logger).Log("msg", "Cloud Map Discovery Refresh Started")

	// Initial credentials loaded from SDK's default credential chain. Such as the environment,
	// shared credentials (~/.aws/credentials), or Instance Role. These credentials will be used
	// to to make the STS Assume Role API, and therefore need the sts:AssumeRole IAM permission
	sess := session.Must(session.NewSession())

	// Create the credentials from AssumeRoleProvider to assume the role provided in config
	creds := stscreds.NewCredentials(sess, d.roleARN)

	// Create service client value configured for credentials from assumed role.
	mapper := servicediscovery.New(sess, &aws.Config{Credentials: creds})

	namespaceFilter := "NAMESPACE_ID"

	tg := &targetgroup.Group{
		Source: "aws",
	}

	// Page through the namespaces in the Cloud Map directory
	err := mapper.ListNamespacesPages(&servicediscovery.ListNamespacesInput{},
		func(namespaceOutputPage *servicediscovery.ListNamespacesOutput, isLastPageOfNamespaces bool) bool {

			for _, namespace := range namespaceOutputPage.Namespaces {

				level.Debug(d.logger).Log("msg", "Getting services for namespace "+*namespace.Name)

				// Build a filter to select any services in the given namespace
				filter := servicediscovery.ServiceFilter{Name: &namespaceFilter, Values: []*string{namespace.Id}}

				err := mapper.ListServicesPages(&servicediscovery.ListServicesInput{Filters: []*servicediscovery.ServiceFilter{&filter}},
					func(servicesOutputPage *servicediscovery.ListServicesOutput, isLastPageOfServices bool) bool {

						for _, service := range servicesOutputPage.Services {

							level.Debug(d.logger).Log("namespace", *namespace.Name, "msg", "Getting instances for service "+*service.Name)

							err := mapper.ListInstancesPages(&servicediscovery.ListInstancesInput{ServiceId: service.Id},
								func(instancesOutputPage *servicediscovery.ListInstancesOutput, isLastPageOfInstances bool) bool {

									for _, instance := range instancesOutputPage.Instances {

										level.Debug(d.logger).Log("namespace", *namespace.Name, "service", *service.Name, "msg", "Discovered instance "+*instance.Id)

										labels := model.LabelSet{
											cloudMapLabelInstanceID: model.LabelValue(*instance.Id),
										}

										if instance.Attributes["AWS_INSTANCE_IPV4"] != nil && instance.Attributes["AWS_INSTANCE_PORT"] != nil {
											addr := net.JoinHostPort(*instance.Attributes["AWS_INSTANCE_IPV4"], *instance.Attributes["AWS_INSTANCE_PORT"])
											labels[model.AddressLabel] = model.LabelValue(addr)
										} else {
											continue
										}

										var accountNumber = ParseAccountNumberFromArn(d.roleARN)
										labels[cloudMapLabelPrivateIP] = model.LabelValue(*instance.Attributes["AWS_INSTANCE_IPV4"])

										//if inst.PrivateDnsName != nil {
										//	labels[cloudMapLabelPrivateDNS] = model.LabelValue(*inst.PrivateDnsName) // Can be built from Service.Name + Namespace.Properties.HttpProperties.HttpName
										//}

										labels[cloudMapLabelAZ] = model.LabelValue(*instance.Attributes["AVAILABILITY_ZONE"])
										labels[cloudMapLabelInstanceState] = model.LabelValue(*instance.Attributes["AWS_INIT_HEALTH_STATUS"])
										labels[cloudMapLabelClusterName] = model.LabelValue(*instance.Attributes["ECS_CLUSTER_NAME"])
										labels[cloudMapLabelTaskDefinitionFamily] = model.LabelValue(*instance.Attributes["ECS_TASK_DEFINITION_FAMILY"])
										labels[cloudMapLabelServiceName] = model.LabelValue(*instance.Attributes["ECS_SERVICE_NAME"])
										labels[cloudMapLabelAccountId] = model.LabelValue(accountNumber)
										labels[cloudMapLabelAccountService] = model.LabelValue(AccountsDetails[accountNumber].Service)
										labels[cloudMapLabelAccountEnvironment] = model.LabelValue(AccountsDetails[accountNumber].Environment)

										tg.Targets = append(tg.Targets, labels)
									}

									return !isLastPageOfInstances
								})

							if err != nil {
								fmt.Println("could not list service instances")
								fmt.Println(err)
								return false
							}
						}

						return !isLastPageOfServices
					})

				if err != nil {
					fmt.Println("could not list namespace services, stopping")
					fmt.Println(err)
					return false
				}
			}

			return !isLastPageOfNamespaces
		})

	if err != nil {
		fmt.Println("could not list directory namespaces")
		fmt.Println(err)
		level.Debug(d.logger).Log("msg", "Cloud Map Discovery Refresh Finished With Exception")
		return nil, errors.Wrap(err, "could not list directory namespaces")
	}

	level.Debug(d.logger).Log("msg", "Cloud Map Discovery Refresh Finished")
	return []*targetgroup.Group{tg}, nil
}

func ParseAccountNumberFromArn(arn string) string {
	arnParts := strings.Split(arn, ":")
	return arnParts[4]
}
