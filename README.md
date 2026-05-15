# ovs-cni-controller

A Kubernetes controller for managing OpenStack Neutron ports using custom resources.

The controller defines two namespaced CRDs:
- `NeutronConfig` (`openstack.humanz.moe/v1`): creates and manages Neutron ports for a given OpenStack network.
- `NeutronIPAddress` (`openstack.humanz.moe/v1`): represents allocated Neutron IP addresses and updates port state on bind/unbind transitions.

## Key behavior

- `NeutronConfig` reads OpenStack auth credentials from a ConfigMap containing `clouds.yaml`.
- It creates Neutron ports for the requested IP addresses and stores the results as `NeutronIPAddress` resources.
- `NeutronIPAddress` resources track port metadata and can transition between `state=unbound` and `state=bound`.
- When a `NeutronConfig` is deleted, its associated `NeutronIPAddress` resources are cleaned up and the OpenStack ports are deleted.

## Prerequisites

- Go v1.24.6+
- Docker or compatible container tool
- `kubectl` installed and configured for your cluster
- Access to a Kubernetes cluster

## Build and Run

Build the controller binary locally:

```sh
make build
```

Run the controller locally against your kubeconfig:

```sh
make run
```

Build and push the controller image:

```sh
make docker-build IMG=<some-registry>/ovs-cni-controller:tag
make docker-push IMG=<some-registry>/ovs-cni-controller:tag
```

## Deploying to a cluster

Install CRDs into the cluster:

```sh
make install
```

Deploy the controller manager using the image specified by `IMG`:

```sh
make deploy IMG=<some-registry>/ovs-cni-controller:tag
```

To remove the controller deployment:

```sh
make undeploy
```

To remove CRDs:

```sh
make uninstall
```

## Using the controller

The controller ships sample resources in `config/samples/`.
Apply them with:

```sh
kubectl apply -k config/samples/
```

### Sample `NeutronConfig`

`config/samples/openstack_v1_neutronconfig.yaml` includes:
- `networkUUID`: the OpenStack network UUID to allocate ports on
- `ips`: a list of IP addresses and/or ranges to assign
- `openStackAuthConfigName`: the name of a ConfigMap in the same namespace containing `clouds.yaml`

The controller expects the ConfigMap to contain valid OpenStack auth data under the key `clouds.yaml`.

### `NeutronIPAddress`

`NeutronIPAddress` resources are typically created and managed by the controller from `NeutronConfig` objects.
They contain port metadata such as `ipAddress`, `subnet`, `macAddress`, and `portID`.

The controller also watches label transitions on `NeutronIPAddress` resources:
- `state=unbound` clears the port `device_id` in OpenStack
- `state=bound` sets the port `device_id` to the pod identifier stored in the resource annotations

## Testing

Run unit tests and verify generated manifests:

```sh
make test
```

Run end-to-end tests using Kind:

```sh
make test-e2e
```

## Development notes

If you update API types or markers, regenerate code and manifests:

```sh
make manifests generate
```

Inspect available make targets:

```sh
make help
```

## License

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

