# syntax=docker/dockerfile:1
FROM ghcr.io/astral-sh/uv:0.11.0 AS uv
FROM python:3.12-slim AS deps
COPY --from=uv /uv /usr/local/bin/uv
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv export --frozen --no-dev --no-emit-project --no-hashes -o requirements.txt

FROM mcr.microsoft.com/azure-functions/python:4-python3.12
ENV AzureWebJobsScriptRoot=/home/site/wwwroot \
    AzureFunctionsJobHost__Logging__Console__IsEnabled=true
COPY --from=deps /app/requirements.txt /requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip pip install -r /requirements.txt
COPY . /home/site/wwwroot
