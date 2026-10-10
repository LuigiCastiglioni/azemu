# syntax=docker/dockerfile:1
FROM mcr.microsoft.com/azure-functions/python:4-python3.11
ENV AzureWebJobsScriptRoot=/home/site/wwwroot \
    AzureFunctionsJobHost__Logging__Console__IsEnabled=true
COPY requirements.txt /requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip pip install -r /requirements.txt
COPY . /home/site/wwwroot
