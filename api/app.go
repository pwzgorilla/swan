package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/mesos"
	"github.com/Dataman-Cloud/swan/types"
	"github.com/Dataman-Cloud/swan/utils"
	"github.com/Dataman-Cloud/swan/utils/fields"
	"github.com/Dataman-Cloud/swan/utils/labels"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
)

func (r *Server) createApp(w http.ResponseWriter, req *http.Request) {
	if err := checkForJSON(req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	compose := req.Form.Get("compose")

	if compose == "" {
		compose = "default"
	}

	var (
		spec      = &version
		id        = fmt.Sprintf("%s.%s.%s.%s", spec.Name, compose, spec.RunAs, r.driver.ClusterName())
		vid       = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
		count     = int(spec.Instances)
		healthSet = spec.HealthCheck != nil && !spec.HealthCheck.IsEmpty()
	)

	var alias string
	if spec.Proxy != nil {
		alias = spec.Proxy.Alias
	}

	app := &types.Application{
		ID:        id,
		Name:      spec.Name,
		Alias:     alias,
		RunAs:     spec.RunAs,
		Cluster:   r.driver.ClusterName(),
		OpStatus:  types.OpStatusCreating,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := r.db.CreateApp(app); err != nil {
		if strings.Contains(err.Error(), "app already exists") {
			http.Error(w, fmt.Sprintf("app %s has already exists", id), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("create app error: %v", err), http.StatusInternalServerError)
		return
	}

	version.ID = vid

	if err := r.db.CreateVersion(id, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	go func(appId string) {
		defer func() {
			app.OpStatus = types.OpStatusNoop
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("update app op-status got error: %v", err)
			}
		}()

		tasks := []*mesos.Task{}

		for i := 0; i < count; i++ {
			var (
				name = fmt.Sprintf("%d.%s", i, appId)
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			cfg := types.NewTaskConfig(spec)

			if cfg.Network != "host" && cfg.Network != "bridge" {
				cfg.Parameters = append(cfg.Parameters, &types.Parameter{
					Key:   "ip",
					Value: spec.IPs[i],
				})

				cfg.IP = spec.IPs[i]
			}

			task := &types.Task{
				ID:      id,
				Name:    name,
				Weight:  100,
				Status:  "pending",
				Healthy: types.TaskHealthyUnset,
				Version: vid,
				Created: time.Now(),
				Updated: time.Now(),
			}

			if healthSet {
				task.Healthy = types.TaskUnHealthy
			}

			if err := r.db.CreateTask(app.ID, task); err != nil {
				log.Errorf("create task failed: %s", err)
				break
			}

			t := mesos.NewTask(
				cfg,
				id,
				name,
			)

			tasks = append(tasks, t)
		}

		results, err := r.driver.LaunchTasks(tasks)
		if err != nil {
			log.Errorf("launch tasks got error")

			for _, t := range tasks {
				task, err := r.db.GetTask(appId, t.GetTaskId().GetValue())
				if err != nil {
					log.Errorf("find task from zk got error: %v", err)
					return
				}

				task.Status = "Failed"
				task.ErrMsg = err.Error()

				if err = r.db.UpdateTask(app.ID, task); err != nil {
					log.Errorf("update task %s status got error: %v", id, err)
				}
			}
		}

		for taskId, launchErr := range results {
			log.Errorf("launch task %s got error: %v", taskId, err)
			task, err := r.db.GetTask(appId, taskId)
			if err != nil {
				log.Errorf("find task from zk got error: %v", err)
				return
			}

			task.Status = "Failed"
			task.ErrMsg = launchErr.Error()

			if err = r.db.UpdateTask(app.ID, task); err != nil {
				log.Errorf("update task %s status got error: %v", id, err)
			}
		}

		return
	}(app.ID)

	writeJSON(w, http.StatusCreated, map[string]string{"Id": app.ID})
}

func (r *Server) listApps(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filters := types.AppFilterOptions{}

	labelsFilter := req.Form.Get("labels")
	if labelsFilter != "" {
		selector, err := labels.Parse(labelsFilter)
		if err != nil {
			http.Error(w, fmt.Sprintf("parse labels %s failed: %v", selector, err), http.StatusBadRequest)
			return
		}
		filters.LabelsSelector = selector
	}

	fieldsFilter := req.Form.Get("fields")
	if fieldsFilter != "" {
		selector, err := fields.ParseSelector(fieldsFilter)
		if err != nil {
			http.Error(w, fmt.Sprintf("parse labels %s failed: %v", selector, err), http.StatusBadRequest)
			return
		}

		filters.FieldsSelector = selector
	}

	rets, err := r.db.ListApps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	apps := make([]*types.Application, 0)
	for _, app := range rets {
		ver, err := r.db.GetVersion(app.ID, app.Version[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if !filterByLabelsSelectors(filters.LabelsSelector, ver.Labels) {
			continue
		}

		if !filterByFieldsSelectors(filters.FieldsSelector, ver) {
			continue
		}

		apps = append(apps, app)
	}

	writeJSON(w, http.StatusOK, apps)
}

func (r *Server) getApp(w http.ResponseWriter, req *http.Request) {
	// TODO(nmg): mux.Vars should be wrapped in context.
	id := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(id)
	if err != nil {
		if strings.Contains(err.Error(), "not exists") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, app)
}

func (r *Server) deleteApp(w http.ResponseWriter, req *http.Request) {
	id := mux.Vars(req)["app_id"]

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s := req.Form.Get("step")

	var (
		step int
		err  error
	)

	if s != "" {
		step, err = strconv.Atoi(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if s == "" {
		step = 5
	}

	app, err := r.db.GetApp(id)
	if err != nil {
		if strings.Contains(err.Error(), "node does not exist") {
			http.Error(w, fmt.Sprintf("app %s not exists", id), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for delete app. %v", err), http.StatusInternalServerError)
		return
	}

	log.Debugf("app %s has %d tasks", app.ID, len(tasks))

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks versions error for delete app. %v", err), http.StatusInternalServerError)
		return
	}

	log.Debugf("app %s has %d versions", app.ID, len(versions))

	app.OpStatus = types.OpStatusDeleting

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to deleting got error: %v", err), http.StatusInternalServerError)
		return
	}

	go func(appId string) {
		var (
			deleting = []*types.Task{}
			counter  = 0
			count    = len(tasks)
			succeed  = 0
		)

		for _, task := range tasks {
			counter++
			deleting = append(deleting, task)

			if len(deleting) >= step || counter >= count {
				log.Debugf("Deleting %d tasks for app %s", len(deleting), appId)
				errs := r.driver.KillTasks(deleting)
				for taskId, err := range errs {
					if err != nil {
						log.Errorf("Kill task %s got error: %v", taskId, err)

						for _, t := range deleting {
							if t.ID == taskId {
								t.ErrMsg = fmt.Sprintf("kill task error: %v", err)
								if err = r.db.UpdateTask(appId, t); err != nil {
									log.Errorf("update task %s got error: %v", t.Name, err)
								}
							}
						}
						continue
					}

					if err := r.db.DeleteTask(taskId); err != nil {
						log.Errorf("Delete task %s got error: %v", taskId, err)

						for _, t := range deleting {
							if t.ID == taskId {
								t.ErrMsg = fmt.Sprintf("delete task error: %v", err)
								if err = r.db.UpdateTask(appId, t); err != nil {
									log.Errorf("update task %s got error: %v", t.Name, err)
								}
							}
						}
						continue
					}
					log.Debugf("task %s deleted succeed", taskId)
					succeed++
				}
				deleting = []*types.Task{}
			}

		}

		if succeed == count {
			for _, version := range versions {
				if err := r.db.DeleteVersion(app.ID, version.ID); err != nil {
					http.Error(w,
						fmt.Sprintf("Delete version %s for app %s got error: %v", version.ID, app.ID, err),
						http.StatusInternalServerError)
					return
				}
			}

			if err := r.db.DeleteApp(app.ID); err != nil {
				log.Errorf("Delete app %s got error: %v", app.ID, err)
			}
		}

	}(app.ID)

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) scaleApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusMethodNotAllowed)
		return
	}

	var scale types.ScalePolicy
	if err := decode(req.Body, &scale); err != nil {
		http.Error(w, fmt.Sprintf("decode scale param error: %v", err), http.StatusBadRequest)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for scale app. %v", err), http.StatusInternalServerError)
		return
	}

	var (
		current = len(tasks)
		goal    = scale.Instances
		ips     = scale.IPs // TODO(nmg): remove after automatic ipam
	)

	if goal < 0 {
		http.Error(w, "the goal count can't be negative", http.StatusBadRequest)
		return
	}

	if goal == current {
		writeJSON(w, http.StatusNotModified, "instances not changed")
		return
	}

	app.OpStatus = types.OpStatusScaling

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to scaling got error: %v", err), http.StatusInternalServerError)
		return
	}

	if goal < current { // scale dwon
		go func() {
			defer func() {
				app.OpStatus = types.OpStatusNoop
				if err := r.db.UpdateApp(app); err != nil {
					log.Errorf("updating app status from scaling to noop got error: %v", err)
				}
			}()

			for i := 1; i <= current-goal; i++ {

				var (
					tname = fmt.Sprintf("%d.%s", current-i, appId)
					tid   string
				)

				for _, task := range tasks {
					if task.Name == tname {
						tid = task.ID
						break
					}
				}

				if tid == "" {
					log.Errorln("no such task name:", tname)
					break
				}

				t, err := r.db.GetTask(appId, tid)
				if err != nil {
					log.Errorln("get db task error:", err)
					break
				}

				if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
					t.Status = "delete failed"
					t.ErrMsg = err.Error()

					if err = r.db.UpdateTask(appId, t); err != nil {
						log.Errorf("update task %s got error: %v", t.Name, err)
					}

					break
				}

				if err := r.db.DeleteTask(t.ID); err != nil {
					log.Errorf("delete task %s got error: %v", t.Name, err)
					break
				}
			}
		}()

		writeJSON(w, http.StatusAccepted, "accepted")
		return
	}

	// scale up
	spec, err := r.db.GetVersion(app.ID, app.Version[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	net := spec.Container.Docker.Network
	if net != "host" && net != "bridge" {
		if len(ips) < int(goal-current) {
			http.Error(w, fmt.Sprintf("IP number cannot be less than the instance number"), http.StatusBadRequest)
			return
		}
	}

	var (
		step      = scale.Step
		onfailure = scale.OnFailure
	)

	go func() {
		defer func() {
			app.OpStatus = types.OpStatusNoop
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("updating app status from scaling to noop got error: %v", err)
			}
		}()

		var (
			tasks     = []*mesos.Task{}
			count     = goal - current
			counter   = 0
			healthSet = spec.HealthCheck != nil && !spec.HealthCheck.IsEmpty()
		)

		for i := current; i < goal; i++ {
			var (
				name = fmt.Sprintf("%d.%s", i, appId)
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
				ver  = spec.ID
			)

			cfg := types.NewTaskConfig(spec)

			if cfg.Network != "host" && cfg.Network != "bridge" {
				cfg.Parameters = append(cfg.Parameters, &types.Parameter{
					Key:   "ip",
					Value: ips[i],
				})

				cfg.IP = ips[i]
			}

			task := &types.Task{
				ID:      id,
				Name:    name,
				Weight:  100,
				Status:  "pending",
				Healthy: types.TaskHealthyUnset,
				Version: ver,
				Created: time.Now(),
				Updated: time.Now(),
			}

			if healthSet {
				task.Healthy = types.TaskUnHealthy
			}

			if err := r.db.CreateTask(appId, task); err != nil {
				log.Errorf("create task failed: %s", err)
				break
			}

			t := mesos.NewTask(
				cfg,
				id,
				name,
			)

			counter++

			tasks = append(tasks, t)

			if len(tasks) >= step || counter >= count {
				results, err := r.driver.LaunchTasks(tasks)
				if err != nil {
					log.Errorf("launch tasks got error: %v", err)

					for _, t := range tasks {
						task, err := r.db.GetTask(appId, t.GetTaskId().GetValue())
						if err != nil {
							log.Errorf("find task from zk got error: %v", err)
							return
						}

						task.Status = "Failed"
						task.ErrMsg = err.Error()

						if err = r.db.UpdateTask(app.ID, task); err != nil {
							log.Errorf("update task %s status got error: %v", task.ID, err)
						}
					}

					if onfailure == types.ScaleFailureStop {
						return
					}
				}

				for taskId, err := range results {
					if err != nil {
						log.Errorf("launch task %s got error: %v", taskId, err)

						if onfailure == types.ScaleFailureStop {
							return
						}
					}
				}

				tasks = []*mesos.Task{}
			}
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) updateApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusMethodNotAllowed)
		return
	}

	newVer := new(types.Version)
	if err := decode(req.Body, newVer); err != nil {
		http.Error(w, fmt.Sprintf("decode update version got error: %v", err), http.StatusInternalServerError)
		return
	}

	if err := newVer.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newVer.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	if err := r.db.CreateVersion(appId, newVer); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for update app. %v", err), http.StatusInternalServerError)
		return
	}

	app.OpStatus = types.OpStatusUpdating

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to rolling-update got error: %v", err.Error), http.StatusInternalServerError)
		return
	}

	var (
		delay     = float64(5)
		onfailure = "stop"
	)

	update := newVer.UpdatePolicy
	if update != nil {
		delay = update.Delay
		onfailure = update.OnFailure
	}

	pending := tasks

	go func() {
		defer func() {
			app.OpStatus = types.OpStatusNoop
			app.Progress = 0
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("updating app status from updating to noop got error: %v", err)
			}
		}()

		var (
			progress  = 0
			healthSet = newVer.HealthCheck != nil && !newVer.HealthCheck.IsEmpty()
		)

		for _, t := range pending {
			progress++
			app.Progress = progress
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("updating app progress got error: %v", err)
			}

			if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
				t.Status = "Failed"
				t.ErrMsg = fmt.Sprintf("kill task for updating :%v", err)

				if err = r.db.UpdateTask(app.ID, t); err != nil {
					log.Errorf("update task %s got error: %v", t.ID, err)
				}

				return
			}

			if err := r.db.DeleteTask(t.ID); err != nil {
				log.Errorf("delete task %s got error: %v", t.ID, err)
				return
			}

			cfg := types.NewTaskConfig(newVer)

			var (
				name = t.Name
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			task := &types.Task{
				ID:      id,
				Name:    name,
				Weight:  100,
				Healthy: types.TaskHealthyUnset,
				Version: newVer.ID,
				Created: t.Created,
				Updated: time.Now(),
			}

			if healthSet {
				task.Healthy = types.TaskUnHealthy
			}

			if err := r.db.CreateTask(appId, task); err != nil {
				log.Errorf("create task failed: %s", err)
				return
			}

			m := mesos.NewTask(cfg, task.ID, task.Name)

			tasks := []*mesos.Task{m}

			results, err := r.driver.LaunchTasks(tasks)
			if err != nil {
				log.Errorf("launch task %s got error: %v", id, err)

				task.Status = "Failed"
				task.ErrMsg = err.Error()

				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", id, err)
				}

				if onfailure == types.UpdateStop {
					return
				}

			}

			for taskId, err := range results {
				if err != nil {
					log.Errorf("launch task %s got error: %v", taskId, err)
				}

				task, err := r.db.GetTask(appId, taskId)
				if err == nil {
					task.OpStatus = types.OpStatusNoop
					if err = r.db.UpdateTask(appId, task); err != nil {
						log.Errorf("update task %s got error: %v", id, err)
					}
				}

				if onfailure == types.UpdateStop {
					return
				}

			}

			// notify proxy

			time.Sleep(time.Duration(delay) * time.Second)
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) canaryUpdate(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusMethodNotAllowed)
		return
	}

	canary := new(types.CanaryUpdateBody)
	if err := decode(req.Body, canary); err != nil {
		http.Error(w, fmt.Sprintf("decode gray publish body got error: %v", err), http.StatusInternalServerError)
		return
	}

	var (
		newVer    = canary.Version
		value     = canary.Value
		count     = canary.Instances
		onfailure = canary.OnFailure
		delay     = canary.Delay
	)

	if value == 0 {
		http.Error(w, "canary value must between (0, 1)", http.StatusInternalServerError)
		return
	}

	if count == 0 {
		count = 1
	}

	if newVer == nil {
		versions, err := r.db.ListVersions(app.ID)
		if err != nil {
			http.Error(w, fmt.Sprintf("list versions got error for canary update. %v", err), http.StatusInternalServerError)
			return
		}

		if len(versions) < 2 {
			http.Error(w, "canary version not specified and app has no new version", http.StatusInternalServerError)
			return
		}

		types.VersionList(versions).Reverse() // TODO

		newVer = versions[0]
	}

	if delay == 0 {
		delay = types.DefaultCanaryUpdateDelay
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for canary update. %v", err), http.StatusInternalServerError)
		return
	}

	app.OpStatus = types.OpStatusUpdating

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to rolling-update got error: %v", err.Error), http.StatusInternalServerError)
		return
	}

	types.TaskList(tasks).Sort() // TODO

	new := 0
	newTasks := make([]*types.Task, 0)
	for _, task := range tasks {
		if task.Version == newVer.ID {
			new++

			newTasks = append(newTasks, task)
		}
	}

	var (
		total = app.TaskCount
		goal  = new + count
	)

	if goal > total {
		goal = total
	}

	newWeight := utils.ComputeWeight(float64(goal), float64(total), value)

	pending := tasks[new:goal]

	go func() {
		defer func() {
			app.OpStatus = types.OpStatusNoop
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("updating app status from updating to noop got error: %v", err)
			}
		}()

		healthSet := newVer.HealthCheck != nil && !newVer.HealthCheck.IsEmpty()

		for _, t := range pending {
			if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
				t.Status = "Failed"
				t.ErrMsg = fmt.Sprintf("kill task for updating :%v", err)

				if err = r.db.UpdateTask(app.ID, t); err != nil {
					log.Errorf("update task %s got error: %v", t.ID, err)
				}

				return
			}

			if err := r.db.DeleteTask(t.ID); err != nil {
				log.Errorf("delete task %s got error: %v", t.ID, err)
				return
			}

			cfg := types.NewTaskConfig(newVer)

			var (
				name = t.Name
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			task := &types.Task{
				ID:      id,
				Name:    name,
				Weight:  newWeight,
				Healthy: types.TaskHealthyUnset,
				Version: newVer.ID,
				Created: t.Created,
				Updated: time.Now(),
			}

			if healthSet {
				task.Healthy = types.TaskUnHealthy
			}

			if err := r.db.CreateTask(appId, task); err != nil {
				log.Errorf("create task failed: %s", err)
				return
			}

			m := mesos.NewTask(cfg, task.ID, task.Name)

			tasks := []*mesos.Task{m}

			results, err := r.driver.LaunchTasks(tasks)
			if err != nil {
				log.Errorf("launch task %s got error: %v", id, err)

				task.Status = "Failed"
				task.ErrMsg = err.Error()

				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", id, err)
				}

				if onfailure == types.CanaryUpdateOnFailureStop {
					return
				}

			}

			for taskId, err := range results {
				if err != nil {
					log.Errorf("launch task %s got error: %v", taskId, err)
				}

				task, err := r.db.GetTask(appId, taskId)
				if err == nil {
					task.OpStatus = types.OpStatusNoop
					if err = r.db.UpdateTask(appId, task); err != nil {
						log.Errorf("update task %s got error: %v", id, err)
					}
				}

				if onfailure == types.CanaryUpdateOnFailureStop {
					return
				}

			}

			// notify proxy

			time.Sleep(time.Duration(delay) * time.Second)
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) rollback(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusMethodNotAllowed)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for rollback app. %v", err), http.StatusInternalServerError)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for rollback app. %v", err), http.StatusInternalServerError)
		return
	}

	app.OpStatus = types.OpStatusRollback

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to rolling-back got error: %v", err.Error), http.StatusInternalServerError)
		return
	}

	verId := req.Form.Get("version")

	var desired *types.Version

	if verId != "" {
		ver, err := r.db.GetVersion(appId, verId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		desired = ver
	}

	if verId == "" {
		if len(versions) < 2 {
			http.Error(w, fmt.Sprintf("no more versions to rollback"), http.StatusInternalServerError)
			return
		}

		types.VersionList(versions).Sort()
		for idx, ver := range versions {
			if ver.ID == app.Version[0] {
				if (idx - 1) < 0 {
					http.Error(w, fmt.Sprintf("version error"), http.StatusInternalServerError)
					return
				}
				desired = versions[idx-1]
			}
		}
	}

	// TODO
	types.TaskList(tasks).Reverse()

	go func() {
		defer func() {
			app.OpStatus = types.OpStatusNoop
			if err := r.db.UpdateApp(app); err != nil {
				log.Errorf("updating app status from rollback to noop got error: %v", err)
			}
		}()

		for _, t := range tasks {
			if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
				t.Status = "Failed"
				t.ErrMsg = fmt.Sprintf("kill task for rollback :%v", err)

				if err = r.db.UpdateTask(appId, t); err != nil {
					log.Errorf("update task %s got error: %v", t.ID, err)
				}

				return
			}

			if err := r.db.DeleteTask(t.ID); err != nil {
				log.Errorf("delete task %s got error: %v", t.ID, err)
				return
			}

			cfg := types.NewTaskConfig(desired)

			var (
				name = t.Name
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			task := &types.Task{
				ID:      id,
				Name:    name,
				Weight:  100,
				Status:  "updating",
				Version: desired.ID,
				Created: t.Created,
				Updated: time.Now(),
			}

			if err := r.db.CreateTask(appId, task); err != nil {
				log.Errorf("create task failed: %s", err)
				return
			}

			m := mesos.NewTask(cfg, task.ID, task.Name)

			tasks := []*mesos.Task{m}

			results, err := r.driver.LaunchTasks(tasks)
			if err != nil {
				log.Errorf("launch task %s got error: %v", task.ID, err)

				task.Status = "Failed"
				task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)

				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", task.ID, err)
				}

				return
			}

			for taskId, err := range results {
				if err != nil {
					log.Errorf("launch task %s got error: %v", taskId, err)
					return
				}

			}

			time.Sleep(2 * time.Second)

		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) updateWeights(w http.ResponseWriter, req *http.Request) {
	var body types.UpdateWeightsBody
	if err := decode(req.Body, &body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var (
		appId   = mux.Vars(req)["app_id"]
		weights = body.Weights
	)

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for update weights. %v", err), http.StatusInternalServerError)
		return
	}

	for n, weight := range weights {
		for _, task := range tasks {
			if task.Index() == n {
				task.Weight = weight

				if err := r.db.UpdateTask(appId, task); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
	}

	// notify proxy

}

func (r *Server) getTasks(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	tasks, err := r.db.ListTasks(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, tasks)
}

func (r *Server) getTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, task)
}

func (r *Server) updateWeight(w http.ResponseWriter, req *http.Request) {
	var body types.UpdateWeightBody
	if err := decode(req.Body, &body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	task.Weight = body.Weight

	if err := r.db.UpdateTask(appId, task); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// notify proxy

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) getVersions(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for get versions. %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, versions)
}

func (r *Server) getVersion(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
		verId = vars["version_id"]
	)

	version, err := r.db.GetVersion(appId, verId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, version)
}

// TODO(nmg): named version should be supported.
func (r *Server) createVersion(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
	)

	if err := checkForJSON(req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	version.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())

	if err := r.db.CreateVersion(appId, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"Id": version.ID})
}

func filterByLabelsSelectors(labelsSelector labels.Selector, appLabels map[string]string) bool {
	if labelsSelector == nil {
		return true
	}

	return labelsSelector.Matches(labels.Set(appLabels))
}

func filterByFieldsSelectors(fieldSelector fields.Selector, ver *types.Version) bool {
	if fieldSelector == nil {
		return true
	}

	// TODO(upccup): there maybe exist better way to got a field/value map
	fieldMap := make(map[string]string)
	fieldMap["runAs"] = ver.RunAs
	return fieldSelector.Matches(fields.Set(fieldMap))
}

func (r *Server) deleteTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.driver.KillTask(task.ID, task.AgentId, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.db.DeleteTask(task.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) deleteTasks(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
	)

	app, err := r.db.GetApp(appId)
	if err != nil {
		if strings.Contains(err.Error(), "node does not exist") {
			http.Error(w, fmt.Sprintf("app %s not exists", appId), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for delete tasks. %v", err), http.StatusInternalServerError)
		return
	}

	for _, task := range tasks {
		go func(task *types.Task, appId string) {
			if err := r.driver.KillTask(task.ID, task.AgentId, false); err != nil {
				log.Errorf("Kill task %s got error: %v", task.ID, err)

				task.OpStatus = fmt.Sprintf("kill task error: %v", err)
				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", task.Name, err)
				}

				return
			}

			if err := r.db.DeleteTask(task.ID); err != nil {
				log.Errorf("Kill task %s got error: %v", task.ID, err)

				task.OpStatus = fmt.Sprintf("delete task error: %v", err)
				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", task.Name, err)
				}

				return
			}

		}(task, app.ID)
	}

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) updateTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	version.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	if err := r.db.CreateVersion(appId, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	t, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
		t.Status = "Failed"
		t.ErrMsg = fmt.Sprintf("kill task for updating :%v", err)

		if err = r.db.UpdateTask(appId, t); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}

		return
	}

	if err := r.db.DeleteTask(t.ID); err != nil {
		log.Errorf("delete task %s got error: %v", t.ID, err)
		return
	}

	cfg := types.NewTaskConfig(&version)

	var (
		name = t.Name
		id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
	)

	task := &types.Task{
		ID:      id,
		Name:    name,
		Weight:  100,
		Status:  "updating",
		Version: version.ID,
		Created: t.Created,
		Updated: time.Now(),
	}

	if err := r.db.CreateTask(appId, task); err != nil {
		log.Errorf("create task failed: %s", err)
		return
	}

	m := mesos.NewTask(cfg, task.ID, task.Name)
	tasks := []*mesos.Task{m}

	results, err := r.driver.LaunchTasks(tasks)
	if err != nil {
		log.Errorf("launch task %s got error: %v", task.ID, err)

		task.Status = "Failed"
		task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)

		if err = r.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for taskId, err := range results {
		if err != nil {
			log.Errorf("launch task %s got error: %v", taskId, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) rollbackTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusMethodNotAllowed)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for rollback task. %v", err), http.StatusInternalServerError)
		return
	}

	app.OpStatus = types.OpStatusRollback

	if err := r.db.UpdateApp(app); err != nil {
		http.Error(w, fmt.Sprintf("updating app opstatus to rolling-back got error: %v", err.Error), http.StatusInternalServerError)
		return
	}

	defer func() {
		app.OpStatus = types.OpStatusNoop
		if err := r.db.UpdateApp(app); err != nil {
			log.Errorf("updating app status from rollback to noop got error: %v", err)
		}
	}()

	verId := req.Form.Get("version")

	var desired *types.Version

	if verId != "" {
		ver, err := r.db.GetVersion(appId, verId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		desired = ver
	}

	if verId == "" {
		if len(versions) < 2 {
			http.Error(w, fmt.Sprintf("no more versions to rollback"), http.StatusInternalServerError)
			return
		}

		// TODO
		types.VersionList(versions).Sort()
		for idx, ver := range versions {
			if ver.ID == app.Version[0] {
				if (idx - 1) < 0 {
					http.Error(w, fmt.Sprintf("version error"), http.StatusInternalServerError)
					return
				}
				desired = versions[idx-1]
			}
		}
	}

	t, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.driver.KillTask(t.ID, t.AgentId, true); err != nil {
		t.Status = "Failed"
		t.ErrMsg = fmt.Sprintf("kill task for rollback :%v", err)

		if err = r.db.UpdateTask(appId, t); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}

		return
	}

	if err := r.db.DeleteTask(t.ID); err != nil {
		log.Errorf("delete task %s got error: %v", t.ID, err)
		return
	}

	cfg := types.NewTaskConfig(desired)

	var (
		name = t.Name
		id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
	)

	task := &types.Task{
		ID:      id,
		Name:    name,
		Weight:  100,
		Status:  "updating",
		Version: desired.ID,
		Created: t.Created,
		Updated: time.Now(),
	}

	if err := r.db.CreateTask(appId, task); err != nil {
		log.Errorf("create task failed: %s", err)
		return
	}

	m := mesos.NewTask(cfg, task.ID, task.Name)

	tasks := []*mesos.Task{m}

	results, err := r.driver.LaunchTasks(tasks)
	if err != nil {
		log.Errorf("launch task %s got error: %v", task.ID, err)

		task.Status = "Failed"
		task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)

		if err = r.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for taskId, err := range results {
		if err != nil {
			log.Errorf("launch task %s got error: %v", taskId, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusAccepted, "accepted")
}
