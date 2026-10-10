# syntax=docker/dockerfile:1
FROM mcr.microsoft.com/azure-functions/node:4-node20
ENV AzureWebJobsScriptRoot=/home/site/wwwroot \
    AzureFunctionsJobHost__Logging__Console__IsEnabled=true
WORKDIR /home/site/wwwroot
COPY package*.json ./
RUN --mount=type=cache,target=/root/.npm npm install
COPY . .
RUN npm run build && npm prune --omit=dev
