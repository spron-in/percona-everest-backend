# Percona everest API integration tests

## Before running tests

Before running tests one needs to have provisioned kubernetes cluster, Everest backend

Running Percona Everest backend. Run these commands in the root of the project

```
   make local-env-up
   make run-debug
```
Running minikube cluster:

**Linux**
```
   make k8s
```
**MacOS**
```
   make k8s-macos
```
Provisioning kubernetes cluster

```
   git clone git@github.com:percona/percona-everest-cli
   cd percona-everest-cli
   go run cmd/everest/main.go install operators --backup.enable=false --everest.endpoint=http://127.0.0.1:8080 --monitoring.enable=false --name=minikube --operator.mongodb=true --operator.postgresql=true --operator.xtradb-cluster=true --skip-wizard
```
Using these commands you'll have installed the following operators

1. Postgres operator
2. PXC operator
3. PSMDB operator
4. DBaaS operator

Make sure all the operators are running:
```
kubectl get dbengines -n percona-everest
```
if not - wait until they do.

After these commands you're ready to run integration tests

## Running integration tests
There are several ways running tests
```
  npx playwright test
    Runs the end-to-end tests.

  npx playwright test tests/test.spec.ts
    Runs the specific tests.

  npx playwright test --ui
    Starts the interactive UI mode.

  npx playwright test --debug
    Runs the tests in debug mode.

  npx playwright codegen
    Auto generate tests with Codegen.
```

or
```
   make init
   make test
```

## CLI commands

There is a `cli` entity which is accessible during test and allows executing CLI commands on the host

Example usage:
```javascript

import { test, expect } from '@fixtures';

test('check mongodb-operator is installed', async ({ cli }) => {
    const output = await cli.execSilent('kubectl get pods --namespace=percona-everest');
    await output.assertSuccess();
    await output.outContains('mongodb-operator');
});
```
