// Copyright 2020 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resources

import (
	"context"
	"fmt"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/mcclient/modulebase"
	"yunion.io/x/onecloud/pkg/util/httputils"
	"yunion.io/x/pkg/utils"

	onecloudv1 "yunion.io/x/onecloud-service-operator/api/v1"
	"yunion.io/x/onecloud-service-operator/pkg/auth"
)

// ReconcileOper describe a Operation about reconcile process.
// It invovles a func, desc and the phase needed.
type ReconcileOper struct {
	Operator OperatorFunc
	OperDesc OperatorDesc
	PrePhase []onecloudv1.ResourcePhase
}

type OperatorFunc func(ctx context.Context) (onecloudv1.ExternalInfoBase, error)

type OperatorDesc struct {
	Name string
	desc []string
}

func (ad *OperatorDesc) Appendf(desc string, params ...interface{}) {
	ad.desc = append(ad.desc, fmt.Sprintf(desc, params...))
}

func (ad *OperatorDesc) Append(resource, from, to string) {
	ad.Appendf(`change "%s" from (%s) to (%s)`, resource, from, to)
}

func (ad *OperatorDesc) Merge(desc OperatorDesc) {
	ad.desc = append(ad.desc, desc.desc...)
}

func (ad *OperatorDesc) String() string {
	return fmt.Sprintf("%s: %s", ad.Name, strings.Join(ad.desc, "; "))
}

// Resoruce describe a onecloud resource, such as:
// VM: SGuest in onecloud
// EIP: SElasticip in onecloud
// AP: AnsiblePlaybook in onecloud
// ...
type Resource string

const (
	ResourceVM   Resource = "VM"
	ResourceEIP  Resource = "EIP"
	ResourceDisk Resource = "Disk"
	ResourceAP   Resource = "AP"
)

// ResourceOperation describe the operation for onecloud resource like create, update, delete and so on.
type ResourceOperation string

// It is clearer to write each ResourceOperation as a constant
const (
	OperCreate       ResourceOperation = "Create"
	OperDelete       ResourceOperation = "Delete"
	OperUpdate       ResourceOperation = "Update"
	OperGet          ResourceOperation = "Get"
	OperList         ResourceOperation = "List"
	OperGetDetails   ResourceOperation = "GetDetails"
	OperGetStatus    ResourceOperation = "GetStatus"
	OperChangeConfig ResourceOperation = "ChangeConfig"
	OperSyncstatus   ResourceOperation = "Syncstatus"
	OperResize       ResourceOperation = "Resize"
	OperChangeBw     ResourceOperation = "ChangeBandwidth"
	OperSetSecgroups ResourceOperation = "SetSecgroups"
	OperStart        ResourceOperation = "Start"
	OperStop         ResourceOperation = "Stop"
)

// Modules describe the correspondence between Resource and modulebase.ResourceManager,
// which is equivalent to onecloud resource client.
var Modules = make(map[Resource]modulebase.ResourceManager)

// Every Resource should call Register to register their modulebase.ResourceManager.
func Register(resource Resource, manager modulebase.ResourceManager) {
	Modules[resource] = manager
}

// SRequestErr encapsulates the error returned by SRequest and implement the interface error
type SRequestErr struct {
	Resource Resource
	Code     int
	Action   string
	Class    string
	Detail   string
}

// Request itself is meaningless, a meaningful Request is generated by
// calling Resource, Operation and DefaultParams.
// A example:
// 	Request.Resource(ResourceVM).Operation(OperGet).Apply(...)
var Request SRequest

// SRequest encapsulates HTTP requests to perform operations on onecloud resources
type SRequest struct {
	resource     Resource
	operation    ResourceOperation
	defautParams *jsonutils.JSONDict
}

func (re SRequestErr) IsNotFound(resource Resource) bool {
	if re.Code != 404 {
		return false
	}
	if re.Resource != resource {
		return false
	}
	return true
}

func (re SRequestErr) IsClientErr() bool {
	return re.Code >= 400 && re.Code != 404 && re.Code < 500
}

func (re SRequestErr) IsServerErr() bool {
	return re.Code >= 500
}

func (re SRequestErr) Error() string {
	return fmt.Sprintf("Exec '%s', %s: %s", re.Action, re.Class, re.Detail)
}

func (r SRequest) Resource(resource Resource) SRequest {
	r.resource = resource
	return r
}

func (r SRequest) Operation(oper ResourceOperation) SRequest {
	r.operation = oper
	return r
}

func (r SRequest) DefaultParams(dict *jsonutils.JSONDict) SRequest {
	r.defautParams = dict
	return r
}

func (r SRequest) ResourceAction() string {
	return fmt.Sprintf("%s%s", r.resource, r.operation)
}

func (r SRequest) Apply(ctx context.Context, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, onecloudv1.ExternalInfoBase, error) {
	status := onecloudv1.ExternalInfoBase{Action: r.ResourceAction()}
	resourceManager, ok := Modules[r.resource]
	if !ok {
		return nil, status, fmt.Errorf("no such resource '%s' in Modules", r.resource)
	}
	var requestFunc func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error)
	switch r.operation {
	case OperCreate:
		requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
			return resourceManager.Create(session, params)
		}
	case OperDelete:
		requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
			return resourceManager.DeleteWithParam(session, id, params, nil)
		}
	case OperUpdate:
		requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
			return resourceManager.Update(session, id, params)
		}
	case OperGet:
		if len(id) > 0 {
			requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
				return resourceManager.Get(session, id, params)
			}
		} else {
			requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
				list, err := resourceManager.List(session, params)
				if err != nil {
					return nil, err
				}
				if len(list.Data) == 0 {
					return nil, httperrors.NewNotFoundError("")
				}
				return list.Data[0], nil
			}
		}
	default:
		if strings.HasPrefix(string(r.operation), string(OperGet)) {
			spec := strings.ToLower(string(r.operation)[3:])
			requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
				return resourceManager.GetSpecific(session, id, spec, params)
			}
		} else {
			action := utils.CamelSplit(string(r.operation), "-")
			requestFunc = func(session *mcclient.ClientSession, id string, params *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
				return resourceManager.PerformAction(session, id, action, params)
			}
		}
	}
	if params == nil {
		params = jsonutils.NewDict()
	}
	ret, err := requestFunc(auth.AdminSession(ctx), id, r.params(params))
	if err != nil {
		client, _ := err.(*httputils.JSONClientError)
		return nil, status, &SRequestErr{
			Resource: r.resource,
			Code:     client.Code,
			Action:   r.ResourceAction(),
			Class:    client.Class,
			Detail:   client.Details,
		}
	}
	status.Status, _ = ret.GetString("status")
	// update ID
	if len(id) > 0 {
		status.Id = id
	} else {
		status.Id, _ = ret.GetString("id")
	}
	return ret, status, nil
}

func mergeJsonDict(dict1, dict2 *jsonutils.JSONDict) *jsonutils.JSONDict {
	for _, k := range dict2.SortedKeys() {
		v, _ := dict2.Get(k)
		dict1.Set(k, v)
	}
	return dict1
}

func (r SRequest) params(dict *jsonutils.JSONDict) *jsonutils.JSONDict {
	if r.defautParams == nil {
		return dict
	}
	return mergeJsonDict(dict, r.defautParams)
}