# Order Service: Business & Technical Overview

## Service Overview
The Order Service is the primary orchestrator of the OtelContext e-commerce ecosystem. It manages the entire lifecycle of a customer order, from initial placement to successful fulfillment and notification. 

## Core Functionality
*   **Order Orchestration:** Coordinates between inventory, payment, and shipping services.
*   **State Management:** Tracks order status (Pending, Paid, Shipped, Cancelled).
*   **Service Integration:** Acts as the central hub for the "Happy Path" simulation.

## Technical Specifications
*   **Primary Port:** 9001
*   **Technology Stack:** Golang with OpenTelemetry (OTLP) instrumentation.

## Performance Characteristics
This service serves as the baseline for system performance. It is designed to be highly responsive but will fail if downstream dependencies (like Payment or Inventory) exhibit critical errors or timeouts.

## Observability
Key metrics tracked for this service include:
*   `order_create_total`: Total number of orders attempted.
*   `order_success_total`: Total number of orders successfully completed.

## Integration Profile
The service communicates with:
*   **Auth Service** (Port 9004) for request validation.
*   **Inventory Service** (Port 9003) for stock checks.
*   **Payment Service** (Port 9002) for financial processing.
*   **Shipping Service** (Port 9006) for logistics.
*   **Notification Service** (Port 9007) for customer updates.

