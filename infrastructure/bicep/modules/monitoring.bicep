param name string
param location string
param alertEmail string

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: name
  location: location
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30 // in-game we charge players for logs. Same joke, real invoice.
  }
}

resource team 'Microsoft.Insights/actionGroups@2023-01-01' = {
  name: '${name}-oncall'
  location: 'global'
  properties: {
    groupShortName: 'lgs-oncall'
    enabled: true
    emailReceivers: [{ name: 'oncall', emailAddress: alertEmail, useCommonAlertSchema: true }]
  }
}

// The one alert that matters: a stalled tick means every game is frozen. The services
// log "tick stalled" with slog, and Container Insights picks that up from stdout.
resource tickStalled 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = {
  name: '${name}-tick-stalled'
  location: location
  properties: {
    displayName: 'Tick barrier is stalling'
    description: 'See docs/runbooks/tick-stall.md'
    severity: 1
    enabled: true
    scopes: [logs.id]
    evaluationFrequency: 'PT5M'
    windowSize: 'PT5M'
    criteria: {
      allOf: [
        {
          query: 'ContainerLogV2 | where ContainerName == "simulation" and LogMessage has "tick stalled"'
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 10
          failingPeriods: { numberOfEvaluationPeriods: 1, minFailingPeriodsToAlert: 1 }
        }
      ]
    }
    actions: { actionGroups: [team.id] }
  }
}

output logAnalyticsId string = logs.id
