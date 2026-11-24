# meshshop — local Kubernetes + Istio microservices playground.
PROFILE    ?= meshshop
NS         ?= meshshop
TAG        ?= dev
LOCAL_PORT ?= 8443
BASE       := https://meshshop.localhost:$(LOCAL_PORT)
SERVICES   := web-bff:services/bff/web \
              shopping:services/business/shopping \
              catalog:services/domain/catalog \
              inventory:services/domain/inventory \
              order:services/domain/order
KUBECTL    := kubectl --context $(PROFILE)

.PHONY: help up cluster certs istio observability identity build deploy rollout status test vuln smoke layering security gateway kibana kibana-password kibana-admin-password users token keycloak-admin traffic logs down clean postman postman-debug debug-ports

help: ## Show targets
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-22s %s\n", $$1, $$2}'

up: cluster certs istio observability build identity deploy rollout ## Everything: cluster, Istio, ELK, Keycloak, images, deploy

# Calico enforces NetworkPolicy (minikube's default bridge CNI ignores it).
cluster: ## Start the minikube cluster (4 CPU / 9 GB, Calico CNI)
	minikube status -p $(PROFILE) >/dev/null 2>&1 || \
	  minikube start -p $(PROFILE) --driver=docker --cpus=4 --memory=9g --kubernetes-version=v1.35.1 --cni=calico

certs: ## Local CA + gateway/Logstash/Filebeat certificates in .certs/ (git-ignored)
	./scripts/certs.sh

istio: certs ## Install Istio via Helm (base, CNI, istiod, ingress gateway) + edge security
	kubectl config use-context $(PROFILE)
	./deploy/istio/install.sh

observability: certs ## Install ELK + APM (ECK operator, Elasticsearch, Kibana, APM, Logstash, Filebeat, OTel Collector)
	./deploy/observability/install.sh
	$(KUBECTL) apply -k deploy/observability/mesh

identity: ## Deploy Keycloak (OIDC provider, realm "meshshop", users alice and bob)
	./deploy/identity/install.sh

# Builds straight into the minikube node's Docker daemon, so pods see new images
# immediately (`minikube image load` silently keeps an image a pod is using).
build: ## Build all service images (and Keycloak) inside the minikube node's Docker
	@eval "$$(minikube -p $(PROFILE) docker-env)" && \
	for s in $(SERVICES); do n=$${s%%:*}; p=$${s#*:}; \
	  echo "==> meshshop/$$n:$(TAG)"; docker build -q --build-arg SERVICE=$$p -t meshshop/$$n:$(TAG) . >/dev/null || exit 1; done && \
	echo "==> meshshop/keycloak:$(TAG)" && docker build -q -t meshshop/keycloak:$(TAG) deploy/identity >/dev/null

deploy: ## Apply Kubernetes + Istio manifests
	$(KUBECTL) apply -k deploy/k8s

rollout: ## Restart deployments (pick up new images) and wait
	$(KUBECTL) -n $(NS) rollout restart deploy
	$(KUBECTL) -n $(NS) rollout status deploy --timeout=180s

status: ## Pods, services and Istio config
	$(KUBECTL) -n $(NS) get pods,svc -L meshshop.io/layer
	$(KUBECTL) -n $(NS) get gateway,virtualservice,destinationrule,authorizationpolicy,peerauthentication,networkpolicy
	$(KUBECTL) -n istio-ingress get requestauthentication,authorizationpolicy,envoyfilter

test: ## Unit tests
	go vet ./... && go test ./...

vuln: ## Scan the Go code, modules and toolchain for known vulnerabilities (fails on findings)
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

gateway: ## Port-forward the ingress gateway's HTTPS port to localhost:$(LOCAL_PORT) (foreground)
	$(KUBECTL) -n istio-ingress port-forward svc/istio-ingressgateway $(LOCAL_PORT):443

smoke: ## End-to-end checks through the gateway (needs `make gateway` running)
	./scripts/smoke.sh $(BASE)

security: ## Security regression checks from the OWASP review (needs `make gateway`)
	./scripts/security.sh $(BASE)

users: ## Print the demo users' Keycloak passwords (alice, bob)
	@for u in alice bob; do printf '%-6s %s\n' $$u "$$($(KUBECTL) -n identity get secret meshshop-demo-users -o jsonpath="{.data.$$u}" | base64 -d)"; done

token: ## Print an access token for a demo user: make token AS=bob (default alice)
	@. scripts/lib.sh && BASE=$(BASE) token $(or $(AS),alice)

keycloak-admin: ## Port-forward the Keycloak admin console to http://localhost:18080/auth/admin (foreground)
	@echo "user admin, password: $$($(KUBECTL) -n identity get secret keycloak-admin -o jsonpath='{.data.password}' | base64 -d)"
	$(KUBECTL) -n identity port-forward deploy/keycloak 18080:8080

kibana: ## Port-forward Kibana to localhost:5601 (foreground); read-only user, see kibana-password
	$(KUBECTL) -n observability port-forward svc/meshshop-kb-http 5601:5601

kibana-password: ## Print the read-only Kibana user (meshshop-viewer) and its password
	@echo "user meshshop-viewer, password: $$($(KUBECTL) -n observability get secret meshshop-viewer-user -o jsonpath='{.data.password}' | base64 -d)"

kibana-admin-password: ## Break-glass only: print the "elastic" superuser's password
	@$(KUBECTL) -n observability get secret meshshop-es-elastic-user -o jsonpath='{.data.elastic}' | base64 -d; echo

traffic: ## Generate a mix of traffic (incl. failures) for Kibana: make traffic N=50
	./scripts/traffic.sh $(BASE) $(or $(N),30)

NEWMAN := $(shell command -v newman || echo "npx -y newman")
POSTMAN := postman/meshshop.postman_collection.json -e postman/meshshop.local.postman_environment.json
# Trust the local CA, and pass the demo users' passwords in (they are not stored in the collection).
NEWMAN_ENV = --ssl-extra-ca-certs .certs/ca.crt \
  --env-var "alicePassword=$$($(KUBECTL) -n identity get secret meshshop-demo-users -o jsonpath='{.data.alice}' | base64 -d)" \
  --env-var "bobPassword=$$($(KUBECTL) -n identity get secret meshshop-demo-users -o jsonpath='{.data.bob}' | base64 -d)"

postman: ## Run the Postman collection's Storefront folder with newman (needs `make gateway`)
	@$(NEWMAN) run $(POSTMAN) $(NEWMAN_ENV) --folder "Storefront (via Istio gateway)"

postman-debug: ## Run the Postman debug folders (needs `make debug-ports`)
	$(NEWMAN) run $(POSTMAN) --folder "Business layer — shopping (debug)" \
	  --folder "Domain — catalog (debug)" --folder "Domain — inventory (debug)" --folder "Domain — order (debug)"

# kubectl port-forward enters the pod directly, past the sidecar and therefore
# past mTLS and every AuthorizationPolicy. Only people allowed pods/portforward
# in meshshop can do this: keep that verb out of shared-cluster roles.
debug-ports: ## DEV ONLY: port-forward internal services to localhost:18081-18084 (bypasses mesh authz)
	@echo "WARNING: these ports bypass the mesh's mTLS and authorization policies (dev only)."
	@echo "shopping :18081  catalog :18082  inventory :18083  order :18084  (Ctrl-C to stop)"
	@trap 'kill 0' INT TERM; \
	$(KUBECTL) -n $(NS) port-forward svc/shopping 18081:8080 >/dev/null & \
	$(KUBECTL) -n $(NS) port-forward svc/catalog 18082:8080 >/dev/null & \
	$(KUBECTL) -n $(NS) port-forward svc/inventory 18083:8080 >/dev/null & \
	$(KUBECTL) -n $(NS) port-forward svc/order 18084:8080 >/dev/null & \
	wait

layering: ## Prove the mesh blocks layer-skipping calls
	./scripts/layering.sh $(PROFILE) $(NS)

logs: ## Tail app logs for one service: make logs SVC=shopping
	$(KUBECTL) -n $(NS) logs -f deploy/$(SVC) -c app

clean: ## Remove the app and Keycloak from the cluster (keeps Istio and ELK)
	$(KUBECTL) delete -k deploy/k8s --ignore-not-found
	$(KUBECTL) delete -k deploy/identity --ignore-not-found

down: ## Delete the minikube cluster
	minikube delete -p $(PROFILE)
