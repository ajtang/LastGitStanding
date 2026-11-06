// The real platform underneath the pretend one. Run per environment:
//   az deployment group create -g rg-lgs-staging -f main.bicep -p staging.bicepparam \
//     -p postgresPassword=$(openssl rand -hex 24)
//
// Deliberately small. It's a game with a leaderboard, so there's one region, no zone
// redundancy and burstable Postgres. The irony of a reliability game running
// single-region is noted in docs/architecture.md, along with what it would cost not to.
targetScope = 'resourceGroup'

@allowed(['staging', 'production'])
param environment string
param location string = resourceGroup().location
param gitRepositoryUrl string
@secure()
param postgresPassword string
param alertEmail string

var name = 'lgs-${environment}'
var unique = uniqueString(resourceGroup().id)

module monitoring 'modules/monitoring.bicep' = {
  name: 'monitoring'
  params: { name: name, location: location, alertEmail: alertEmail }
}

module aks 'modules/aks.bicep' = {
  name: 'aks'
  params: {
    name: name
    location: location
    logAnalyticsId: monitoring.outputs.logAnalyticsId
    environment: environment
    gitRepositoryUrl: gitRepositoryUrl
  }
}

module data 'modules/data.bicep' = {
  name: 'data'
  params: {
    name: name
    unique: unique
    location: location
    postgresPassword: postgresPassword
    oidcIssuer: aks.outputs.oidcIssuer
    logAnalyticsId: monitoring.outputs.logAnalyticsId
  }
}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: 'lgs${unique}'
  location: location
  sku: { name: 'Basic' }
  properties: { adminUserEnabled: false }
}

var acrPull = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')

resource kubeletCanPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, name, acrPull)
  scope: registry
  properties: {
    principalId: aks.outputs.kubeletObjectId
    roleDefinitionId: acrPull
    principalType: 'ServicePrincipal'
  }
}

output registry string = registry.properties.loginServer
output keyVaultName string = data.outputs.keyVaultName
output workloadClientId string = data.outputs.workloadClientId
output tenantId string = tenant().tenantId
