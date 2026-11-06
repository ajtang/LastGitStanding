using 'main.bicep'

param environment = 'production'
param gitRepositoryUrl = 'https://github.com/ajtang/LastGitStanding'
param alertEmail = 'oncall@lastgitstanding.example'
param postgresPassword = readEnvironmentVariable('LGS_POSTGRES_PASSWORD')
