/*
 Copyright 2022 The KubeVela Authors.

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

package sync

import (
	"context"
	"errors"

	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/utils"

	"github.com/kubevela/velaux/pkg/server/domain/model"
	"github.com/kubevela/velaux/pkg/server/domain/service"
	"github.com/kubevela/velaux/pkg/server/infrastructure/datastore"
	v1 "github.com/kubevela/velaux/pkg/server/interfaces/api/dto/v1"
	"github.com/kubevela/velaux/pkg/server/utils/bcode"
)

// DataStoreApp is a memory struct that describes the model of an application in datastore
type DataStoreApp struct {
	Project  *v1.CreateProjectRequest
	AppMeta  *model.Application
	Env      *model.Env
	Eb       *model.EnvBinding
	Comps    []*model.ApplicationComponent
	Policies []*model.ApplicationPolicy
	Workflow *model.Workflow
	Targets  []*model.Target
	Record   *model.WorkflowRecord
	Revision *model.ApplicationRevision
}

// StoreProject will create project for synced application
func StoreProject(ctx context.Context, project v1.CreateProjectRequest, ds datastore.DataStore, projectService service.ProjectService) error {
	err := ds.Get(ctx, &model.Project{Name: project.Name})
	if err == nil {
		// it means the record already exists, don't need to add anything
		return nil
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	if projectService != nil {
		project.Owner = model.DefaultAdminUserName
		project.Description = model.AutoGenProj
		_, err := projectService.CreateProject(ctx, project)
		return err
	}
	return nil
}

// StoreAppMeta will sync application metadata from CR to datastore
func StoreAppMeta(ctx context.Context, app *DataStoreApp, ds datastore.DataStore) error {
	oldApp := &model.Application{Name: app.AppMeta.Name}
	err := ds.Get(ctx, oldApp)
	if err == nil {
		// it means the record already exists
		app.AppMeta.CreateTime = oldApp.CreateTime
		return ds.Put(ctx, app.AppMeta)
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	return ds.Add(ctx, app.AppMeta)
}

// StoreEnv will sync application namespace from CR to datastore env, one namespace belongs to one env
func StoreEnv(ctx context.Context, app *DataStoreApp, ds datastore.DataStore, envService service.EnvService) error {
	curEnv := &model.Env{Name: app.Env.Name}
	err := ds.Get(ctx, curEnv)
	if err == nil {
		// it means the record already exists, compare the targets
		if utils.EqualSlice(curEnv.Targets, app.Env.Targets) {
			return nil
		}
		app.Env.CreateTime = curEnv.CreateTime
		return ds.Put(ctx, app.Env)
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	_, err = envService.CreateEnv(ctx, v1.CreateEnvRequest{
		Name:                app.Env.Name,
		Alias:               app.Env.Alias,
		Description:         app.Env.Description,
		Project:             app.Env.Project,
		Namespace:           app.Env.Namespace,
		Targets:             app.Env.Targets,
		AllowTargetConflict: true,
	})
	if err != nil && !errors.Is(err, bcode.ErrEnvAlreadyExists) {
		return err
	}
	return nil
}

// StoreEnvBinding will add envbinding for application CR one application one envbinding
func StoreEnvBinding(ctx context.Context, eb *model.EnvBinding, ds datastore.DataStore) error {
	err := ds.Get(ctx, eb)
	if err == nil {
		// it means the record already exists, don't need to add anything
		return nil
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	return ds.Add(ctx, eb)
}

// StoreComponents will sync application components from CR to datastore
func StoreComponents(ctx context.Context, appPrimaryKey string, expComps []*model.ApplicationComponent, ds datastore.DataStore) error {

	// list the existing components in datastore
	originComps, err := ds.List(ctx, &model.ApplicationComponent{AppPrimaryKey: appPrimaryKey}, &datastore.ListOptions{})
	if err != nil {
		return err
	}
	var originCompNames []string
	var existComponentMap = make(map[string]*model.ApplicationComponent)
	for i := range originComps {
		comp := originComps[i].(*model.ApplicationComponent)
		originCompNames = append(originCompNames, comp.Name)
		existComponentMap[comp.Name] = comp
	}

	var targetCompNames []string
	for _, comp := range expComps {
		targetCompNames = append(targetCompNames, comp.Name)
	}

	_, readyToDelete, readyToAdd := utils.ThreeWaySliceCompare(originCompNames, targetCompNames)

	// delete the components that not belongs to the new app
	for _, entity := range originComps {
		comp := entity.(*model.ApplicationComponent)
		// we only compare for components that automatically generated by sync process.
		if comp.Creator != model.AutoGenComp {
			continue
		}
		if !utils.StringsContain(readyToDelete, comp.Name) {
			continue
		}
		if err := ds.Delete(ctx, comp); err != nil {
			if errors.Is(err, datastore.ErrRecordNotExist) {
				continue
			}
			klog.Warningf("delete comp %s for app %s  failure %s", comp.Name, appPrimaryKey, err.Error())
		}
	}

	// add or update new app's components for datastore
	for _, comp := range expComps {
		if utils.StringsContain(readyToAdd, comp.Name) {
			err = ds.Add(ctx, comp)
		} else {
			if old := existComponentMap[comp.Name]; old != nil {
				comp.CreateTime = old.CreateTime
			}
			err = ds.Put(ctx, comp)
		}
		if err != nil {
			klog.Warningf("convert comp %s for app %s into datastore failure %s", comp.Name, utils.Sanitize(appPrimaryKey), err.Error())
			return err
		}
	}
	return nil
}

// StorePolicy will add/update/delete policies, we don't delete ref policy
func StorePolicy(ctx context.Context, appPrimaryKey string, expPolicies []*model.ApplicationPolicy, ds datastore.DataStore) error {
	// list the existing policies for this app in datastore
	originPolicies, err := ds.List(ctx, &model.ApplicationPolicy{AppPrimaryKey: appPrimaryKey}, &datastore.ListOptions{})
	if err != nil {
		return err
	}
	var originPolicyNames []string
	var policyMap = make(map[string]*model.ApplicationPolicy)
	for i := range originPolicies {
		plc := originPolicies[i].(*model.ApplicationPolicy)
		originPolicyNames = append(originPolicyNames, plc.Name)
		policyMap[plc.Name] = plc
	}

	var targetPLCNames []string
	for _, plc := range expPolicies {
		targetPLCNames = append(targetPLCNames, plc.Name)
	}

	_, readyToDelete, readyToAdd := utils.ThreeWaySliceCompare(originPolicyNames, targetPLCNames)

	// delete the components that not belongs to the new app
	for _, entity := range originPolicies {
		plc := entity.(*model.ApplicationPolicy)
		// we only compare for policies that automatically generated by sync process
		// and the policy should not be ref ones.

		if plc.Creator != model.AutoGenPolicy {
			continue
		}
		if !utils.StringsContain(readyToDelete, plc.Name) {
			continue
		}
		if err := ds.Delete(ctx, plc); err != nil {
			if errors.Is(err, datastore.ErrRecordNotExist) {
				continue
			}
			klog.Warningf("delete policy %s for app %s failure %s", plc.Name, appPrimaryKey, err.Error())
		}
	}

	// add or update new app's policies for datastore
	for _, plc := range expPolicies {
		if utils.StringsContain(readyToAdd, plc.Name) {
			err = ds.Add(ctx, plc)
		} else {
			if existPolicy := policyMap[plc.Name]; existPolicy != nil {
				plc.CreateTime = existPolicy.CreateTime
			}
			err = ds.Put(ctx, plc)
		}
		if err != nil {
			klog.Warningf("convert policy %s for app %s into datastore failure %s", plc.Name, utils.Sanitize(appPrimaryKey), err.Error())
			return err
		}
	}
	return nil
}

// StoreWorkflow will sync workflow application CR to datastore, it updates the only one workflow from the application with specified name
func StoreWorkflow(ctx context.Context, dsApp *DataStoreApp, ds datastore.DataStore) error {
	old := &model.Workflow{AppPrimaryKey: dsApp.AppMeta.Name, Name: dsApp.Workflow.Name}
	err := ds.Get(ctx, old)
	if err == nil {
		dsApp.Workflow.CreateTime = old.CreateTime
		// it means the record already exists, update it
		return ds.Put(ctx, dsApp.Workflow)
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	return ds.Add(ctx, dsApp.Workflow)
}

// StoreWorkflowRecord will sync workflow status to datastore.
func StoreWorkflowRecord(ctx context.Context, dsApp *DataStoreApp, ds datastore.DataStore) error {
	if dsApp.Record == nil {
		return nil
	}
	records, err := ds.List(ctx, &model.WorkflowRecord{AppPrimaryKey: dsApp.AppMeta.Name, Name: dsApp.Record.Name}, nil)
	if err == nil && len(records) > 0 {
		return nil
	}
	if err != nil {
		// other database error, return it
		return err
	}
	return ds.Add(ctx, dsApp.Record)
}

// StoreApplicationRevision will sync the application revision to datastore.
func StoreApplicationRevision(ctx context.Context, dsApp *DataStoreApp, ds datastore.DataStore) error {
	if dsApp.Revision == nil {
		return nil
	}
	old := &model.ApplicationRevision{AppPrimaryKey: dsApp.AppMeta.Name, Version: dsApp.Revision.Version}
	err := ds.Get(ctx, old)
	if err == nil {
		dsApp.Revision.CreateTime = old.CreateTime
		return ds.Put(ctx, dsApp.Revision)
	}
	if !errors.Is(err, datastore.ErrRecordNotExist) {
		// other database error, return it
		return err
	}
	return ds.Add(ctx, dsApp.Revision)
}

// StoreTargets will sync targets from application CR to datastore
func StoreTargets(ctx context.Context, dsApp *DataStoreApp, ds datastore.DataStore, targetService service.TargetService) error {
	for _, t := range dsApp.Targets {
		err := ds.Get(ctx, t)
		if err == nil {
			continue
		}
		if !errors.Is(err, datastore.ErrRecordNotExist) {
			// other database error, return it
			return err
		}
		_, err = targetService.CreateTarget(ctx, v1.CreateTargetRequest{
			Name:        t.Name,
			Alias:       t.Alias,
			Project:     t.Project,
			Description: t.Description,
			Cluster:     (*v1.ClusterTarget)(t.Cluster),
			Variable:    t.Variable,
		})
		if err != nil && !errors.Is(err, bcode.ErrTargetExist) {
			return err
		}
	}
	return nil
}
