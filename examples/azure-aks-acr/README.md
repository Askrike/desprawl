# AKS and a web app pulling from one container registry

An AKS cluster and a Linux web app share one user-assigned managed identity,
which has `AcrPull` on a container registry. The role assignment grants
access to the identity, so it shows as access from both of the identity's
users to the registry.
