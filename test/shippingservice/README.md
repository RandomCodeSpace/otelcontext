# Shipping Service: Business & Technical Overview

## Service Overview
The Shipping Service manages the logistical fulfillment of successfully paid orders. It generates tracking numbers and coordinates with the virtual delivery system.

## Core Functionality
*   **Tracking Generation:** Creates unique tracking identifiers for all fulfilled orders.
*   **Logistics Coordination:** Communicates with the simulated carrier API.

## Technical Specifications
*   **Primary Port:** 9006
*   **Technology Stack:** Golang with OTLP tracing.

## Performance Characteristics
Shipping operations are asynchronous and slow by nature. 
*   **Fixed Processing Time:** Every ship request includes a deliberate **500ms delay** to simulate the overhead of logistic communication and label generation.

## Observability
*   `ship_request_total`: Total count of shipment attempts.
*   `tracking_issued_total`: Count of unique tracking numbers successfully generated.

## Integration Profile
Primary consumer is the **Order Service** (Port 9001).
