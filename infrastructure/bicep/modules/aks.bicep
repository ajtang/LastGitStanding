param name string
param location string
param logAnalyticsId string
param environment string
param gitRepositoryUrl string

resource aks 'Microsoft.ContainerService/managedClusters@2024-05-01' = {
  name: name
  location: location
  identity: { type: 'SystemAssigned' }
  sku: { name: 'Base', tier: environment == 'production' ? 'Standard' : 'Free' }
  properties: {
    dnsPrefix: name
    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        vmSize: 'Standard_D2s_v5'
        count: environment == 'production' ? 3 : 2
        osType: 'Linux'
        enableAutoScaling: false
      }
    ]
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkPolicy: 'cilium'
      networkDataplane: 'cilium'
    }
    oidcIssuerProfile: { enabled: true }
    securityProfile: { workloadIdentity: { enabled: true } }
    ingressProfile: { webAppRouting: { enabled: true } }
    addonProfiles: {
      omsagent: { enabled: true, config: { logAnalyticsWorkspaceResourceID: logAnalyticsId } }
      azureKeyvaultSecretsProvider: { enabled: true, config: { enableSecretRotation: 'true' } }
    }
    azureMonitorProfile: { metrics: { enabled: true } }
  }
}

resource flux 'Microsoft.KubernetesConfiguration/extensions@2023-05-01' = {
  name: 'flux'
  scope: aks
  properties: {
    extensionType: 'microsoft.flux'
    autoUpgradeMinorVersion: true
  }
}

resource gitops 'Microsoft.KubernetesConfiguration/fluxConfigurations@2023-05-01' = {
  name: 'clusters'
  scope: aks
  dependsOn: [flux]
  properties: {
    scope: 'cluster'
    namespace: 'flux-system'
    sourceKind: 'GitRepository'
    gitRepository: {
      url: gitRepositoryUrl
      repositoryRef: { branch: 'main' }
      syncIntervalInSeconds: 60
    }
    kustomizations: {
      cluster: { path: './clusters/${environment}', prune: true, syncIntervalInSeconds: 300 }
    }
  }
}

output oidcIssuer string = aks.properties.oidcIssuerProfile.issuerURL
output kubeletObjectId string = aks.properties.identityProfile.kubeletidentity.objectId
