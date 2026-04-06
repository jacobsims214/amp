# Go Engineer Skill

The go-engineer skill provides comprehensive guidelines and best practices for writing production-quality Go backend code. This skill is designed for developers working with Go 1.22+ and covers essential patterns for error handling, database operations with pgx/v5, HTTP handlers, routing with chi, concurrent programming, and testing strategies.

## Purpose

This skill serves as a reference for implementing idiomatic Go patterns that promote code maintainability, reliability, and performance. It establishes clear conventions for common development tasks and helps prevent subtle bugs that can arise from incorrect patterns. The guidelines are particularly focused on backend services that interact with PostgreSQL databases and expose HTTP APIs.

## When to Use

Use this skill when:
- Writing new Go backend code or services
- Reviewing Go code for adherence to project standards
- Setting up database operations with PostgreSQL
- Implementing HTTP APIs with routing
- Designing concurrent operations
- Writing unit and integration tests
- Establishing error handling strategies across a codebase

## Error Handling

Proper error handling is critical for building robust Go applications. The go-engineer skill emphasizes wrapping errors with context to preserve the error chain while providing meaningful information about where failures occur. Always use `fmt.Errorf` with the `%w` verb to wrap errors, never string concatenation. This approach enables the use of `errors.Is()` for checking error types without relying on fragile string comparisons.

Sentinel errors should be defined as package-level variables using `errors.New()` rather than creating new error instances at each call site. This allows callers to check for specific error conditions using `errors.Is()`. Custom error types should be created when additional context or methods are needed, avoiding the `Err` prefix convention in type names.

The skill recommends handling errors in a single place—either logging or returning, but not both. This prevents duplicate logging and keeps error handling logic clean and predictable. When wrapping errors, the format should be `"operation: %w"` where the operation describes what was being attempted, and the wrapped error provides the underlying cause.

## Database Operations with pgx/v5

For PostgreSQL operations, the go-engineer skill advocates using pgx/v5 with specific patterns that improve code clarity and reduce bugs. Positional parameters (`$1`, `$2`, etc.) should always be used rather than named parameters, as this is the standard pgx approach.

When querying data, use `pgx.CollectRows()` with the `pgx.RowToAddrOfStructByPos[T]` mapper to automatically map query results to struct pointers. This eliminates manual `.Scan()` loops and reduces the chance of mapping errors. Always defer the closing of rows immediately after querying to prevent resource leaks.

Database connection pools should be injected as dependencies rather than created within individual handlers or functions. This promotes connection reuse and allows for centralized pool configuration. When working with transactions, prefer `pool.BeginTx()` with explicit `pgx.TxOptions` over the simpler `pool.Begin()` to have explicit control over transaction behavior.

## HTTP Handlers

HTTP handler implementation follows specific patterns to ensure consistent behavior and correct HTTP semantics. Context should always be the first parameter in handler functions, never stored as a struct field. This makes the request lifecycle explicit and easier to reason about.

Content-Type headers must be set before calling `WriteHeader()` or writing any response body, as headers set after the response has started are ignored. Status codes should be written before the response body, never after. The recommended approach is to use `json.NewEncoder(w).Encode(v)` for JSON responses rather than manually marshaling with `json.Marshal()` and then writing to the response.

Error handling in HTTP handlers should use early returns whenever possible. When an error occurs, write the appropriate error response and return immediately rather than continuing execution. This pattern makes the control flow clearer and prevents accidental processing after errors.

## Chi Router

The chi router is the recommended HTTP router for Go services in this project. The pattern involves creating a router instance and applying middleware using the `Use()` method. Common middleware includes logging and recovery handlers that should be applied globally or to specific route groups.

Route groups allow for organizing routes by feature or applying common middleware to subsets of routes. Authentication middleware can be applied to entire route groups, and custom middleware chains can be created using the `With()` method. The router supports standard HTTP methods (Get, Post, Put, Delete, etc.) and URL parameters that can be extracted using `chi.URLParam()`.

## Concurrency

Concurrent operations should leverage the `errgroup.WithContext()` pattern for managing groups of goroutines with proper context cancellation and error aggregation. When using errgroup, always set a limit on the number of concurrent operations using `g.SetLimit()` to prevent resource exhaustion from unbounded goroutine creation.

Every errgroup should have its `Wait()` method called to ensure all goroutines complete and any errors are properly returned. Never use fire-and-forget goroutines without waiting for them to complete, as this can lead to incomplete operations and resource leaks.

When launching goroutines inside loops, always capture the loop variable by creating a local copy before the goroutine is launched. This prevents the common bug where all goroutines end up referencing the same loop variable, which may have changed by the time the goroutine executes.

## Testing

Testing in Go follows table-driven test patterns as the standard approach. Each test should define a slice of test cases, each containing the test name, input parameters, and expected results. The test function then iterates through these cases using `t.Run()` with subtests, which provides better test output and allows individual test cases to fail independently.

The testify library's `require` package should be used for assertions that should stop test execution when they fail, such as checking for expected errors. The `assert` package should be used for assertions that should continue test execution, such as comparing multiple values. This distinction allows tests to report all failures rather than stopping at the first assertion.

Mocking should be done against interfaces rather than concrete types. Define minimal interfaces that capture only the behavior needed for testing, then create mock implementations of those interfaces. This approach promotes loose coupling and makes testing more maintainable, as changes to concrete types that don't affect the interface won't require test updates.

## Examples

The skill references an examples file that contains complete, working code samples demonstrating all the patterns discussed. These examples serve as templates that can be adapted for specific use cases while maintaining the established best practices.

## Conclusion

The go-engineer skill provides a comprehensive framework for writing high-quality Go backend code. By following these patterns consistently, developers can build reliable, maintainable services that leverage Go's strengths while avoiding common pitfalls. The guidelines cover the full development lifecycle from error handling through testing, ensuring that code quality is maintained at every stage.
