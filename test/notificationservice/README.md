# Notification Service: Business & Technical Overview

## Service Overview
The Notification Service is the primary communication hub for OtelContext. it handles multi-channel messages (email, SMS) to keep customers informed about their order status changes.

## Core Functionality
*   **Dispatch Engine:** Sends notifications based on triggers from the Order Service.
*   **Message Templating:** Manages various message types (Receipts, Shipped alerts, Cancellation notices).

## Technical Specifications
*   **Primary Port:** 9007
*   **Technology Stack:** Golang.

## Performance Characteristics
This service is a high-availability utility with minimal latency:
*   **Low Overhead:** Optimized for rapid fire-and-forget message dispatch.

## Observability
*   `notification_sent_total`: Total count of messages pushed to the dispatch queue.
*   `notification_error_total`: Count of failures in the delivery chain.

## Integration Profile
Primary consumer is the **Order Service** (Port 9001).

