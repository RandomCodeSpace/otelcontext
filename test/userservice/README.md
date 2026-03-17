# User Service: Business & Technical Overview

## Service Overview
The User Service acts as the System of Record for all customer profile data. It provides identity, preferences, and account-level information to the rest of the microservices.

## Core Functionality
*   **Profile Retrieval:** Provides customer data for order personalization.
*   **Identity Management:** Serves as the source of truth for the **Auth Service**.

## Technical Specifications
*   **Primary Port:** 9005
*   **Technology Stack:** Golang.

## Performance Characteristics
This service exhibits "Flaky" behavior to test system robustness:
*   **Simulated Downtime:** The service is configured with a **15% probability of failure** for every incoming request. This forces upstream callers to implement retry logic, circuit breakers, and graceful degradation.

## Observability
*   `user_fetch_success`: Track successful data retrieval.
*   `user_fetch_error`: Track the frequency of simulated 15% failures.

## Integration Profile
Primary downstream consumer is the **Auth Service** (Port 9004).
