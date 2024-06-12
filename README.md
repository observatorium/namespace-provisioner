# Namespace Provisioner

Namespace Provisioner is a tool for self-servicing the creation of short-lived namespaces in a Kubernetes cluster.

## Design

There are two main threads in the Namespace Provisioner:
1. An API server fulfilling requests to create and delete namespaces; and
1. A Kubernetes controller watching namespaces for deletion.

### Authentication

The Namespace Provisioner requires all requests to the API to be authenticated.
Currently, the API only allows clients to authenticate via a bearer token, which must be specified at run-time with the `--token=<token>` flag.

### Privileges

The Namespace Provisioner provides the client with a Kubeconfig to operate the Namespaces it creates and binds a ClusterRole it to give it privileges.
The ClusterRole is bound to the Kubeconfig using a RoleBinding, scoping the permissions down to only the newly created Namespace.
By default, the Namespace Provisioner uses a ClusterRole named `namespace-provisioner-grant`, which grants no permissions to the subject.
To control the permissions granted to the returned Kubeconfig, administrators can edit the `namespace-provisioner-grant` ClusterRole or change the target ClusterRole by specifying a different `--cluster-role=<name>` flag passed to the Namespace Provisioner.

### API Server

The Namespace Provisioner runs an API server over HTTP that exposes two API endpoints:
1. Namespace creation; and
1. Namespace deletion.

#### Namespace Creation - POST /api/v1/namespace

The Namespace creation endpoint accepts the following optional query parameters:
1. `ttl`: the time in seconds that the Namespace should exist in the Kubernetes cluster; if 0 is given, then the Namespace Provisioner’s default lifetime is applied.
All provisioned Namespaces will be labeled with a Unix timestamp equal to the current time plus this duration; and
1. `url`; the URL of the Kubernetes API that the generated Kubeconfig should use.

The Namespace creation endpoint responds with the following data:
1. A Kubeconfig with scoped privileges for the provisioned Namespace using the provided RBAC Role and the Kubernetes API URL provided in the creation request.

In order to generate the Kubeconfig to fulfill the request, the Namespace provisioner first generates a ServiceAccount for the new Namespace, binds the specified RBAC role to it, and finally uses the certificate and token for the ServiceAccount to produce a Kubeconfig.

#### Namespace Deletion - DELETE /api/v1/namespace/<name>

The Namespace deletion endpoint determines what namespace to delete from the parameter in the URL path.

### Kubernetes Controller
The Namespace provisioner runs a Kubernetes controller to manage all of the resources it creates. Chiefly, it maintains a control loop to watch four resources:
1. Namepaces;
1. ServiceAccounts;
1. Roles; and
1. RoleBindings

The controller runs filtered indexers for each of these resources that limit the watched resources to only those that are labelled with `controller.observatorium.io=namespace-provisioner`.
Any time that a resource with this label is modified, the controller ensures that all of the resources for the corresponding Namespace are correct.

Each resource provisioned for a Namespace creation request is also labelled with a Unix timestamp for the expiration time of the Namespace.
Whenever the controller re-syncs, it checks the expiration timestamp of the resource and deletes it if it has expired.
