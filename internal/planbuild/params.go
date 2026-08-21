/*
Copyright 2026.

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

package planbuild

import (
	"encoding/json"
	"fmt"
	"maps"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// jsonToMap decodes j's raw JSON into a map, treating a nil j or an empty
// Raw (Params never set) as an empty map rather than an error - the same
// "absent means no params" reading ImageSpec.Params and MachineSpec.Params's
// doc comments give the field.
func jsonToMap(j *apiextensionsv1.JSON) (map[string]any, error) {
	m := map[string]any{}
	if j == nil || len(j.Raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(j.Raw, &m); err != nil {
		return nil, fmt.Errorf("parsing params JSON: %w", err)
	}
	return m, nil
}

// mergeParams merges imageParams and machineParams into a single map,
// imageParams first and machineParams overriding on key collision - the
// order ImageSpec.Params and MachineSpec.Params's doc comments fix.
func mergeParams(imageParams, machineParams *apiextensionsv1.JSON) (map[string]any, error) {
	merged, err := jsonToMap(imageParams)
	if err != nil {
		return nil, fmt.Errorf("image params: %w", err)
	}
	fromMachine, err := jsonToMap(machineParams)
	if err != nil {
		return nil, fmt.Errorf("machine params: %w", err)
	}
	maps.Copy(merged, fromMachine)
	return merged, nil
}
