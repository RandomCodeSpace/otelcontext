# Payment Service: Business & Technical Overview

## Service Overview
The Payment Service handles the financial settlement of orders. It processes transactions, verifies funds, and manages the ledger for all purchase operations within the simulation.

## Core Functionality
*   **Transaction Processing:** Authorizes and captures payments for orders.
*   **Ledger Management:** Maintains a record of financial success and failure states.
*   **Fraud Detection (Simulated):** Implements basic validation logic to simulate real-world processing gates.

## Technical Specifications
*   **Primary Port:** 9002
*   **Technology Stack:** Golang with custom Prometheus metrics and OTLP tracing.

## Performance Characteristics
Payments are inherently sensitive to latency. In this environment:
*   **Dependency Failure:** The Payment Service will intentionally fail if the **Inventory Service** (Port 9003) or **Auth Service** (Port 9004) is unreachable, simulating cascading failures in the financial checkout flow.

## Observability
*   `payment_success_total`: Cumulative count of successful settlements.
*   `payment_failure_total`: Cumulative count of declined or failed transactions.

## Integration Profile
Integrates primarily with:
*   **Order Service** (Port 9001) as its primary consumer.
*   **Auth Service** (Port 9004) for identity verification.
*   **Inventory Service** (Port 9003) for coordinate stock reservation checks.
