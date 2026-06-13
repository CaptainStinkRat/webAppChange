#!/bin/sh
set -e

echo "=== CodeGen App Entrypoint ==="

# Wait for Ollama to be ready
echo "Waiting for Ollama..."
until curl -s http://ollama:11434/api/tags > /dev/null 2>&1; do
    sleep 2
done
echo "Ollama is ready."

# Pull the model (cached if already exists)
echo "Pulling model: ${LLM_MODEL}"
curl -s -X POST http://ollama:11434/api/pull \
    -d "{\"name\": \"${LLM_MODEL}\", \"stream\": false}" > /dev/null
echo "Model ${LLM_MODEL} ready."

# Start the Go server
echo "Starting server on port ${APP_PORT}..."
exec /app/server
