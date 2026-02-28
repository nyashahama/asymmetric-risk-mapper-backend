# Asymmetric Risk Mapper Backend

Backend service for the Asymmetric Risk Mapper application. This is a Go-based REST API that powers risk analysis and mapping functionality, built with modern tooling and containerization support.

## 🎯 Overview

The Asymmetric Risk Mapper Backend is a production-ready Go service that provides APIs for analyzing and mapping asymmetric risks. It features:

- **Modern Go Stack**: Built with Go 1.22.2 for performance and reliability
- **REST API**: Clean HTTP endpoints using Chi router
- **PostgreSQL Database**: Robust data persistence with migration support
- **Docker Support**: Containerized deployment with Docker and Docker Compose
- **Type-Safe SQL**: Generated SQL code using sqlc for compile-time safety
- **Integration Ready**: Pre-configured integrations for Stripe, Anthropic, DeepSeek, and Resend APIs

## 📋 Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.22.2 |
| Web Framework | Chi v5 |
| Database | PostgreSQL 16 |
| SQL Generation | sqlc |
| Container | Docker |
| Orchestration | Docker Compose |
| Key Libraries | stripe-go, uuid, pq, godotenv |

## 🚀 Quick Start

### Prerequisites

- Docker and Docker Compose
- Or: Go 1.22.2+ and PostgreSQL 16

### Using Docker Compose

Start the complete development stack (API + PostgreSQL):

```bash
docker compose up
