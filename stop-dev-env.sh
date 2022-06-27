#!/bin/bash

docker stop managed-gitops-postgres
docker stop managed-gitops-pgadmin

docker rm managed-gitops-postgres
docker rm managed-gitops-pgadmin
