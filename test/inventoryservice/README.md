# Inventory Service: Business & Technical Overview

## Service Overview
The Inventory Service is responsible for maintaining accurate stock levels across the product catalog. It manages stock reservations and triggers replenishment logic during high-demand periods.

## Core Functionality
*   **Stock Checking:** Provides real-time availability data for the Order and Payment services.
*   **Reservation Logic:** Holds stock during the checkout window to prevent over-selling.
*   **Restocking Simulation:** Automatically replenishes items as they reach zero to ensure continuous simulation throughput.

## Technical Specifications
*   **Primary Port:** 9003
*   **Technology Stack:** Golang with integrated health checks.

## Performance Characteristics
Designed to simulate "Hot Keys" and heavy concurrent access:
*   **Lock Contention:** High-volume traffic to specific items will mimic database locking and increased processing time.

## Observability
*   `stock_check_count`: Total validation requests received.
*   `stock_depletion_count`: Tracks how often items hit zero stock.

## Integration Profile
Serves as a shared dependency for:
*   **Order Service** (Port 9001)
*   **Payment Service** (Port 9002)
