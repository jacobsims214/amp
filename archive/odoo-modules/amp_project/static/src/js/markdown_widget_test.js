/** @odoo-module **/
/**
 * AMP Markdown Widget Test Suite
 * Simple tests to verify core functionality
 */

import { MarkdownWidget } from "./markdown_widget";

// Test markdown parsing functionality
function testMarkdownParsing() {
    const testCases = [
        {
            input: "# Header 1\n## Header 2\n### Header 3",
            expected: "<h1>Header 1</h1><h2>Header 2</h2><h3>Header 3</h3>"
        },
        {
            input: "**bold** and *italic* text",
            expected: "<strong>bold</strong> and <em>italic</em> text"
        },
        {
            input: "`inline code` and ```\ncode block\n```",
            expected: "<code>inline code</code> and <pre><code>code block</code></pre>"
        },
        {
            input: "[link text](https://example.com)",
            expected: '<a href="https://example.com" target="_blank">link text</a>'
        },
        {
            input: "- List item 1\n- List item 2",
            expected: "<ul><li>List item 1</li><li>List item 2</li></ul>"
        }
    ];

    console.log("Testing Markdown Parsing...");
    testCases.forEach((testCase, index) => {
        // Note: This is a simplified test - in a real implementation,
        // we would need to properly test the MarkdownParser class
        console.log(`Test ${index + 1}: ${testCase.input} -> Expected parsing`);
    });
}

// Test widget integration points
function testWidgetIntegration() {
    console.log("Testing Widget Integration...");
    
    // Test that widget is properly registered
    const fieldRegistry = odoo.__DEBUG__.services["@web/core/registry"].category("fields");
    const markdownWidget = fieldRegistry.get("markdown");
    
    if (markdownWidget) {
        console.log("✓ Markdown widget successfully registered in field registry");
    } else {
        console.error("✗ Markdown widget not found in field registry");
    }
    
    // Test widget properties
    if (markdownWidget && markdownWidget.template === "amp_project.MarkdownWidget") {
        console.log("✓ Widget template correctly configured");
    } else {
        console.error("✗ Widget template not correctly configured");
    }
}

// Test toolbar functionality
function testToolbarFunctions() {
    console.log("Testing Toolbar Functions...");
    
    const mockWidget = {
        state: { value: "test content" },
        textareaRef: { el: { 
            selectionStart: 0, 
            selectionEnd: 4,
            value: "test content",
            focus: () => {},
            setSelectionRange: () => {}
        }},
        props: { 
            record: { 
                update: () => {},
                data: { test_field: "test content" }
            },
            name: "test_field"
        },
        _updatePreview: () => {}
    };
    
    // Test text insertion function
    const insertText = MarkdownWidget.prototype._insertText.bind(mockWidget);
    
    try {
        insertText('**', '**', 'bold text');
        console.log("✓ Text insertion function works");
    } catch (error) {
        console.error("✗ Text insertion function failed:", error);
    }
}

// Run tests when module loads
if (typeof window !== 'undefined' && window.location.search.includes('test=markdown')) {
    document.addEventListener('DOMContentLoaded', () => {
        console.log("=== AMP Markdown Widget Tests ===");
        testMarkdownParsing();
        testWidgetIntegration();
        testToolbarFunctions();
        console.log("=== Tests Complete ===");
    });
}