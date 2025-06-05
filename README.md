# Groq Chat API

A high-performance WebSocket-based chat API that leverages Groq's LLM capabilities to provide real-time chat interactions. This project demonstrates advanced handling of concurrent WebSocket connections with robust error handling and performance optimizations.

## Features

- Real-time chat interactions via WebSocket
- Integration with Groq's LLM API
- High concurrency support (50+ simultaneous connections)
- Robust error handling and recovery mechanisms
- Comprehensive test coverage
- Docker support for easy deployment

## Prerequisites

- Go 1.x or higher
- Docker (optional, for containerized deployment)
- Groq API Key

## API Endpoints

### WebSocket Chat Handler
**Endpoint:** `/ws/chat`

This endpoint establishes a WebSocket connection for real-time chat interactions.

**Headers Required:**
```
X-Session-ID: 'any string'
```

**Request Body Format:**
```json
{
    "messages":[
        {
            "content":"Yes, I was making sure that you are working, how is it going?",
            "role":"user"
        }
    ],
    "stream":false
}
```

### Health Check
**Endpoint:** `/health`

A simple health check endpoint to verify the API's operational status.

**Method:** GET

**Response:**
```json
{
    "status": "ok",
    "timestamp": "2024-03-21T10:00:00Z"
}
```

## Running the Project

### Method 1: Local Development

1. Clone the repository:
```bash
git clone https://github.com/emam3/groq-chat-api.git
cd groq-chat-api
```

2. Install dependencies:
```bash
go mod download
```

3. Set up your Groq API key:
```bash
# For Windows PowerShell
$env:GROQ_API_KEY='YOUR_API_KEY'

# For Linux/MacOS
export GROQ_API_KEY='YOUR_API_KEY'
```

4. Run the application:
```bash
go run main.go
```

The server will start on `http://localhost:8000`

### Method 2: Docker Deployment

1. Pull the Docker image:
```bash
docker pull nouraldin/groq-chat-api
```

2. Run the container:
```bash
docker run -p 8000:8000 -e GROQ_API_KEY='YOUR_API_KEY' nouraldin/groq-chat-api
```

Alternatively, you can use docker-compose:

1. Set your Groq API key in your environment:
```bash
# For Windows PowerShell
$env:GROQ_API_KEY='YOUR_API_KEY'

# For Linux/MacOS
export GROQ_API_KEY='YOUR_API_KEY'
```

2. Run with docker-compose:
```bash
docker-compose up --build
```

### Method 3: Using Deployed Version

The API is also available at: https://groq-chat-api-qtkx.onrender.com/

## Technical Implementation

### High Concurrency Handling

The WSChatHandler implements several advanced techniques to handle high concurrency:

1. **Caching System**: Implements an efficient caching mechanism to reduce redundant API calls and improve response times.

2. **Rate Limiting**: Implements a sophisticated rate limiter to prevent API abuse and ensure fair resource distribution.

3. **Queue System**: Utilizes a queue system to manage and process requests efficiently, preventing system overload.

4. **Circuit Breaker**: Implements a circuit breaker pattern to handle failures gracefully and prevent cascading failures.

For detailed implementation, please refer to the code in the `handlers` directory.

## Testing

The project includes comprehensive integration tests that cover approximately 86% of the current functionality. Tests can be run using:

```bash
go test ./...
```

Test coverage is automatically uploaded to [Codecov](https://codecov.io) after each test run, providing detailed insights into code coverage metrics and trends.

## CI/CD

The project includes two GitHub Actions workflows:

1. **Test Workflow**: Automatically runs all tests on every push and pull request to ensure code quality.

2. **Deployment Workflow**: Builds a Docker image and deploys it to Render for production use. 