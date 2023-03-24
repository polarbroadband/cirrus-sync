/*
Copyright 2023.

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

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"go.uber.org/zap"

	dns "github.com/polarbroadband/cirrus-dns/api/v1alpha1"
	git "github.com/polarbroadband/rp1/gitlib"
)

var (
	PORT_LISTENING_REST_API = os.Getenv("PORT_LISTENING_REST_API")
	GITOPS_API_TOKEN        = os.Getenv("GITOPS_API_TOKEN")
	GIT_API_URL             = os.Getenv("GIT_API_URL")
	GIT_ACCESS_TOKEN        = os.Getenv("GIT_ACCESS_TOKEN")

	scheme = runtime.NewScheme()

	plog *zap.Logger
	log  *zap.SugaredLogger
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dns.AddToScheme(scheme))

	plog, _ = zap.NewDevelopment()
	log = plog.Sugar()
}

type Host struct {
	Name string
}

type Webhook struct {
	Host
	K8s client.Client
	Git *git.GitLab
}

func (h *Host) Healtz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"host": h.Name, "status": "ready"})
}

func (whk *Webhook) SyncAll() error {
	wlog := log.With("event", "cirrus-init-sync", "git", whk.Git.URL)
	wlog.Infow("start", "cr", "Dns")
	return nil
}

func (whk *Webhook) WebhookPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"func": "Cirrus Webhook", "status": "ready", "instance": whk.Host.Name})
}

func (whk *Webhook) DnsSync(w http.ResponseWriter, r *http.Request) {
	wlog := log.With("event", "GitLab webhook", "git", r.Header.Get("X-Gitlab-Instance"), "handler", r.Host+r.URL.String())
	w.Header().Set("Content-Type", "application/json")

	if event := r.Header.Get("X-Gitlab-Event"); event != "Push Hook" {
		wlog.Errorw("invalid webhook event", "event", event)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if token := r.Header.Get("X-Gitlab-Token"); token != GITOPS_API_TOKEN {
		wlog.Errorw("invalid webhook token", "token", token)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	wlog.Infow("recvd webhook push event")
	trigger := git.GitLabWebhookEvent{}
	if err := json.NewDecoder(r.Body).Decode(&trigger); err != nil {
		wlog.Errorw("unable to parse webhook event", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	commit := trigger.GetCheckoutCommit(whk.Git)
	if commit == nil {
		wlog.Errorw("unable to locate checkout commit in webhook event")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	decode := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode

	files, err := commit.GetAddedFiles()
	if err != nil {
		wlog.Errorw("unable to load added files", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	for name, content := range files {
		obj, gvk, err := decode(*content, nil, nil)
		if err != nil {
			wlog.Errorw("unable to decode customer resource", "filename", name, err)
			http.Error(w, http.StatusText(http.StatusUnprocessableEntity), http.StatusUnprocessableEntity)
			return
		}
		if gvk.Kind == "Dns" {
			cr := obj.(*dns.Dns)
			cr.Annotations = map[string]string{"dns.cirrus.ocloud.dev": commit.SHA}
			if err := whk.K8s.Create(context.Background(), cr); err != nil {
				wlog.Errorw("unable to create resource", "gvk", gvk, "name", cr.Name, err)
			} else {
				wlog.Infow("resource create success", "gvk", gvk, "name", cr.Name)
			}
		} else {
			wlog.Errorw("invalid Dns resource", "filename", name)
			http.Error(w, http.StatusText(http.StatusNotAcceptable), http.StatusNotAcceptable)
			return
		}
	}

	files, err = commit.GetModifiedFiles()
	if err != nil {
		wlog.Errorw("unable to load modified files", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	for name, content := range files {
		obj, gvk, err := decode(*content, nil, nil)
		if err != nil {
			wlog.Errorw("unable to decode customer resource", "filename", name, err)
			http.Error(w, http.StatusText(http.StatusUnprocessableEntity), http.StatusUnprocessableEntity)
			return
		}
		if gvk.Kind == "Dns" {
			cr := obj.(*dns.Dns)
			current := &dns.Dns{}
			target := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Name}
			if err := whk.K8s.Get(context.Background(), target, current); err != nil {
				wlog.Errorw("unable to get k8s object", err, "cr", target)
			}
			current.Spec = cr.Spec
			current.Annotations["dns.cirrus.ocloud.dev"] = commit.SHA
			if err := whk.K8s.Update(context.Background(), current); err != nil {
				wlog.Errorw("unable to update resource", "gvk", gvk, "name", cr.Name, err)
				// if err := whk.K8s.Patch(context.Background(), current, client.MergeFrom(cr)); err != nil {
				// 	wlog.Errorw("unable to update resource", "gvk", gvk, "name", cr.Name, err)
			} else {
				wlog.Infow("resource update success", "gvk", gvk, "name", cr.Name)
			}
		} else {
			wlog.Errorw("invalid Dns resource", "filename", name)
			http.Error(w, http.StatusText(http.StatusNotAcceptable), http.StatusNotAcceptable)
			return
		}
	}

	files, err = commit.GetRemovedFiles()
	if err != nil {
		wlog.Errorw("unable to load removed files", err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	for name, content := range files {
		obj, gvk, err := decode(*content, nil, nil)
		if err != nil {
			wlog.Errorw("unable to decode customer resource", "filename", name, err)
			http.Error(w, http.StatusText(http.StatusUnprocessableEntity), http.StatusUnprocessableEntity)
			return
		}
		if gvk.Kind == "Dns" {
			cr := obj.(*dns.Dns)
			current := &dns.Dns{}
			target := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Name}
			if err := whk.K8s.Get(context.Background(), target, current); err != nil {
				wlog.Errorw("unable to get k8s object", err, "cr", target)
			}
			if err := whk.K8s.Delete(context.Background(), current); err != nil {
				wlog.Errorw("unable to remove resource", "gvk", gvk, "name", cr.Name, err)
			} else {
				wlog.Infow("resource remove success", "gvk", gvk, "name", cr.Name)
			}
		} else {
			wlog.Errorw("invalid Dns resource", "filename", name)
			http.Error(w, http.StatusText(http.StatusNotAcceptable), http.StatusNotAcceptable)
			return
		}

	}
	// cr := &dns.Dns{}
	// target := client.ObjectKey{Namespace: "dns-system", Name: "dns-sample"}
	// log.Infow("get resource Dns", "object", target)
	// if err := whk.K8s.Get(context.Background(), target, cr); err != nil {
	// 	log.Errorw("unable to get k8s object", err, "cr", target)
	// }
	json.NewEncoder(w).Encode("success")
}

func ApiRestPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"func": "Cirrus API-REST", "status": "ready"})
}

func main() {
	hostName, err := os.Hostname()
	if err != nil {
		log.Fatal("unable to get CirrusSync instance name", err)
	}
	host := Host{hostName}

	log.Infow("CirrusSync initializing ...", "instance", hostName)

	// k8s client
	clnt, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalw("unable to create k8s client", err)
	}
	webhook := Webhook{
		Host: host,
		K8s:  clnt,
		Git: &git.GitLab{
			URL:         GIT_API_URL,
			AccessToken: GIT_ACCESS_TOKEN,
			Client: &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
					},
				},
			},
		},
	}

	// sync all
	if err := webhook.SyncAll(); err != nil {
		log.Fatalw("unable to init-sync Cirrus customer resource", err)
	}

	// healtz probe
	http.HandleFunc("/healtz", host.Healtz)

	// webhooks
	http.HandleFunc("/webhook/ping", webhook.WebhookPing)
	http.HandleFunc("/webhook/dns", webhook.DnsSync)

	// open API
	http.HandleFunc("/api/ping", ApiRestPing)

	go func() {
		log.Infow("starting CirrusSync API server", "port", PORT_LISTENING_REST_API)
		if err := http.ListenAndServe(":"+PORT_LISTENING_REST_API, nil); err != nil {
			log.Fatalw("unable to start CirrusSync REST API server", err)
		}
	}()

	hold := make(chan struct{})
	<-hold
}
