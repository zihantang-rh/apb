//
// Copyright (c) 2018 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/automationbroker/bundle-lib/bundle"
	"github.com/automationbroker/bundle-lib/clients"
	"github.com/automationbroker/bundle-lib/runtime"
	"github.com/lestrrat/go-jsschema/validator"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
	"k8s.io/api/core/v1"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunBundle will run the bundle's action in the given namespace
func RunBundle(action string, ns string, args []string) {
	bundleName := args[0]
	specs := []*bundle.Spec{}
	var targetSpec *bundle.Spec
	targets := []string{ns}
	pn := fmt.Sprintf("bundle-%s", uuid.New())
	viper.UnmarshalKey("Specs", &specs)
	for _, s := range specs {
		if s.FQName == bundleName {
			targetSpec = s
		}
	}
	if targetSpec == nil {
		log.Errorf("Didn't find supplied APB: %v\n", bundleName)
		return
	}

	plan := selectPlan(targetSpec)
	if plan.Name == "" {
		log.Warning("Did not find a selected plan")
	} else {
		fmt.Printf("Plan: %v\n", plan.Name)
	}

	params, err := selectParameters(plan)
	if err != nil {
		log.Errorf("Error validating selected parameters: %v", err)
		return
	}
	extraVars, err := createExtraVars(ns, &params, plan)
	if err != nil {
		log.Errorf("Error creating extravars: %v\n", err)
		return
	}

	labels := map[string]string{
		"bundle-fqname":   targetSpec.FQName,
		"bundle-action":   action,
		"bundle-pod-name": pn,
	}
	ec := runtime.ExecutionContext{
		BundleName: pn,
		Targets:    targets,
		Metadata:   labels,
		Action:     action,
		Image:      targetSpec.Image,
		Account:    "apb",
		Location:   ns,
		ExtraVars:  extraVars,
	}

	k8scli, err := clients.Kubernetes()
	if err != nil {
		panic(err.Error())
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ec.BundleName,
			Labels: ec.Metadata,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  pn,
					Image: ec.Image,
					Args: []string{
						ec.Action,
						"--extra-vars",
						ec.ExtraVars,
					},
					Env:             createPodEnv(ec),
					ImagePullPolicy: "IfNotPresent",
				},
			},
			RestartPolicy:      v1.RestartPolicyNever,
			ServiceAccountName: ec.Account,
		},
	}
	_, err = k8scli.Client.CoreV1().Pods(ns).Create(pod)
	if err != nil {
		log.Errorf("Failed to create pod: %v", err)
		return
	}
	fmt.Printf("Successfully created pod [%v] to %s [%v] in namespace [%v]\n", pn, ec.Action, bundleName, ns)
	return
}

func selectPlan(spec *bundle.Spec) bundle.Plan {
	var planName string
	if len(spec.Plans) > 1 {
		fmt.Printf("List of available plans:\n")
		for _, plan := range spec.Plans {
			fmt.Printf("name: %v\n", plan.Name)
		}
		fmt.Printf("Enter name of plan you'd like to deploy: ")
		fmt.Scanln(&planName)
	} else {
		return spec.Plans[0]
	}
	for _, plan := range spec.Plans {
		if plan.Name == planName {
			return plan
		}
	}
	return bundle.Plan{}
}

func selectParameters(plan bundle.Plan) (bundle.Parameters, error) {
	schemaPlan, err := bundle.ConvertPlansToSchema([]bundle.Plan{plan})
	if err != nil {
		log.Errorf("Error converting bundle plans to JSON Schema: %v", err)
		return nil, err
	}
	planSchema := schemaPlan[0].Schemas
	schemaParams := planSchema.ServiceInstance.Create["parameters"]
	params := bundle.Parameters{}
	for _, param := range plan.Parameters {
		var paramDefault interface{}
		if param.Default != nil {
			paramDefault = param.Default
		}
		var check bool = true
		for check {
			var paramInput string
			fmt.Printf("Enter value for parameter [%v], default: [%v]: ", param.Name, paramDefault)
			fmt.Scanln(&paramInput)
			if paramInput == "" {
				switch paramDefault.(type) {
				case int:
					paramInput = strconv.Itoa(paramDefault.(int))
				case string:
					paramInput = paramDefault.(string)
				case float64:
					paramInput = strconv.FormatFloat(paramDefault.(float64), 'f', 0, 32)
				}
			}
			if param.Required == true && paramInput == "" {
				fmt.Printf("Parameter [%v] is required. Please try again.\n", param.Name)
				continue
			}

			if len(param.Enum) > 0 {
				if !contains(param.Enum, paramInput) {
					fmt.Printf("[%v] is not a valid option. Available options: %v\n", paramInput, param.Enum)
					continue
				}
			}

			input, err := pruneInput(paramInput, param)
			if err != nil {
				fmt.Printf("Error accepting input: %v\n", err)
				fmt.Println("Please try again")
			} else {
				check = false
				params.Add(param.Name, input)
			}
		}
	}
	v := validator.New(schemaParams)
	if err := v.Validate(params); err != nil {
		log.Debugf("Error validating parameters: %v", err)
		return nil, err
	}

	log.Debugf("Params: %v\n", params)
	return params, nil
}

func createPodEnv(executionContext runtime.ExecutionContext) []v1.EnvVar {
	podEnv := []v1.EnvVar{
		v1.EnvVar{
			Name: "POD_NAME",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		v1.EnvVar{
			Name: "POD_NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
	}
	return podEnv
}

func createExtraVars(targetNamespace string, parameters *bundle.Parameters, plan bundle.Plan) (string, error) {
	var paramsCopy bundle.Parameters
	if parameters != nil && *parameters != nil {
		paramsCopy = *parameters
	} else {
		paramsCopy = make(bundle.Parameters)
	}

	if targetNamespace != "" {
		paramsCopy["namespace"] = targetNamespace
	}

	paramsCopy["cluster"] = "openshift"
	paramsCopy["_apb_plan_id"] = plan.Name
	paramsCopy["_apb_service_instance_id"] = "1234"
	paramsCopy["_apb_service_class_id"] = "1234"
	paramsCopy["in_cluster"] = false
	extraVars, err := json.Marshal(paramsCopy)
	return string(extraVars), err
}

func pruneInput(input string, param bundle.ParameterDescriptor) (interface{}, error) {
	var output interface{}
	var err error
	switch param.Type {
	case "string":
		output = input
	case "enum":
		output = input
	case "bool":
		output, err = strconv.ParseBool(input)
		if err != nil {
			return nil, errors.New("Input must be a boolean")
		}
	case "int":
		output, err = strconv.ParseInt(input, 0, 0)
		if err != nil {
			return nil, errors.New("Input must be an integer")
		}
	default:
		output = input
	}
	return output, nil
}

func contains(s []string, t string) bool {
	for _, str := range s {
		if str == t {
			return true
		}
	}
	return false
}