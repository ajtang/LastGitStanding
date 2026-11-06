param name string
param unique string
param location string
@secure()
param postgresPassword string
param oidcIssuer string
param logAnalyticsId string

resource postgres 'Microsoft.DBforPostgreSQL/flexibleServers@2023-12-01-preview' = {
  name: '${name}-${unique}'
  location: location
  sku: { name: 'Standard_B1ms', tier: 'Burstable' }
  properties: {
    version: '16'
    administratorLogin: 'lgs'
    administratorLoginPassword: postgresPassword
    storage: { storageSizeGB: 32 }
    backup: { backupRetentionDays: 7 }
  }
}

resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2023-12-01-preview' = {
  parent: postgres
  name: 'lgs'
}

// Reachable from Azure services only, which is broader than a private endpoint and a
// whole VNet cheaper. Revisit when the leaderboard is worth attacking.
resource allowAzure 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2023-12-01-preview' = {
  parent: postgres
  name: 'AllowAzureServices'
  properties: { startIpAddress: '0.0.0.0', endIpAddress: '0.0.0.0' }
}

// If this service starts screaming, the tick loop is the thing that stops. Standard
// tier because it needs the SLA; persistence stays off because a week of abandoned
// game state isn't worth paying to replay.
resource redis 'Microsoft.Cache/redis@2023-08-01' = {
  name: '${name}-${unique}'
  location: location
  properties: {
    sku: { name: 'Standard', family: 'C', capacity: 1 }
    enableNonSslPort: false
    minimumTlsVersion: '1.2'
  }
}

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: 'lgs${unique}'
  location: location
  properties: {
    tenantId: tenant().tenantId
    sku: { family: 'A', name: 'standard' }
    enableRbacAuthorization: true
  }
}

resource redisUrl 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = {
  parent: vault
  name: 'redis-url'
  properties: { value: 'rediss://:${redis.listKeys().primaryKey}@${redis.properties.hostName}:${redis.properties.sslPort}' }
}

resource databaseUrl 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = {
  parent: vault
  name: 'database-url'
  properties: { value: 'postgres://lgs:${postgresPassword}@${postgres.properties.fullyQualifiedDomainName}:5432/lgs?sslmode=require' }
}

resource workload 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${name}-workload'
  location: location
}

resource federated 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: workload
  name: 'lgs-secrets'
  properties: {
    issuer: oidcIssuer
    subject: 'system:serviceaccount:lgs:lgs-secrets'
    audiences: ['api://AzureADTokenExchange']
  }
}

var secretsUser = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')

resource canRead 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vault.id, workload.id, secretsUser)
  scope: vault
  properties: {
    principalId: workload.properties.principalId
    roleDefinitionId: secretsUser
    principalType: 'ServicePrincipal'
  }
}

resource redisLogs 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  name: 'to-log-analytics'
  scope: redis
  properties: {
    workspaceId: logAnalyticsId
    metrics: [{ category: 'AllMetrics', enabled: true }]
  }
}

output keyVaultName string = vault.name
output workloadClientId string = workload.properties.clientId
