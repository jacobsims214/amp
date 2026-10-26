{{/* Client IP in the audit log (KB decisions/audit-log-v2.md; #2430, #2469).

     AMP's boundary is its own Envoy. Envoy decides who the client is and
     tells the services in the X-Amp-Client-Ip request header; the services
     believe that header only from the addresses in clientIP.trustedProxies.
     Whatever sits in front of Envoy (a load balancer, an ingress controller,
     a CDN, a tunnel, a service mesh) belongs to the deployment, not to this
     chart: describe it to Envoy with envoy.clientIP. */}}

{{/* amp.trustedProxiesEnv: AMP_TRUSTED_PROXIES for every listener that
     writes audit rows (amp-api REST and MCP, authadmin, mcpoauth, amp-relay).
     These are the addresses Envoy reaches the services from (its pods, or a
     mesh sidecar's inbound address such as 127.0.0.6); from them, and only
     from them, X-Amp-Client-Ip is the client. Empty: nothing is trusted and
     each row records its TCP peer (Envoy). Never list an address a client
     can connect to the services from. */}}
{{- define "amp.trustedProxiesEnv" -}}
- name: AMP_TRUSTED_PROXIES
  value: {{ join "," (.Values.clientIP.trustedProxies | default list) | quote }}
{{- end -}}

{{/* amp.envoyClientIPCIDRs: envoy.clientIP.trustedCIDRs as Envoy CidrRanges.
     A bare address is a single host. */}}
{{- define "amp.envoyClientIPCIDRs" -}}
{{- range (.Values.envoy.clientIP.trustedCIDRs | default list) }}
{{- $parts := splitList "/" (toString .) }}
{{- $addr := first $parts }}
{{- $len := ternary 128 32 (contains ":" $addr) }}
{{- if gt (len $parts) 1 }}{{ $len = atoi (index $parts 1) }}{{ end }}
- { address_prefix: {{ $addr | quote }}, prefix_len: {{ $len }} }
{{- end }}
{{- end -}}

{{/* amp.envoyClientIP: the client-IP settings of Envoy's
     HttpConnectionManager, the same on every listener that routes to AMP.

     Two ways to describe what is in front of Envoy (envoy.clientIP):
     - xffNumTrustedHops (default 0): use_remote_address, so Envoy appends
       its TCP peer to X-Forwarded-For. With 0 the client is Envoy's TCP
       peer and every client-sent X-Forwarded-For entry is ignored. With N,
       the client is the Nth X-Forwarded-For entry from the right: set N to
       the number of proxies in front of Envoy that append to it. Anything
       that can reach Envoy directly can then put any address there, so keep
       everything but those proxies away from Envoy (networkPolicy.envoyIngress
       or your own network rules), or use trustedCIDRs.
     - trustedCIDRs (Envoy 1.32 or later): Envoy's xff original-IP
       detection. Only when the TCP
       peer is in these CIDRs does Envoy read X-Forwarded-For, right to left
       past the trusted entries; the first other address is the client. Any
       other peer is the client itself. This mode can't be combined with
       use_remote_address, so Envoy then trusts a client-sent x-request-id
       (it is still validated by the services) and doesn't strip x-envoy-*
       request headers (AMP reads none).

     In hop mode no address counts as internal, so Envoy always replaces a
     client-sent x-request-id and strips x-envoy-* headers, whatever the
     peer. Side effect: X-Forwarded-Proto becomes Envoy's own scheme; nothing
     in AMP reads it. */}}
{{- define "amp.envoyClientIP" -}}
{{- $c := .Values.envoy.clientIP | default dict -}}
{{- $hops := int ($c.xffNumTrustedHops | default 0) -}}
{{- if lt $hops 0 }}{{ fail "envoy.clientIP.xffNumTrustedHops must be 0 or more" }}{{ end -}}
{{- if $c.trustedCIDRs -}}
{{- if gt $hops 0 }}{{ fail "envoy.clientIP: set xffNumTrustedHops or trustedCIDRs, not both" }}{{ end -}}
{{- $v := regexFind "^v?1[.][0-9]+" (toString .Values.envoy.tag) -}}
{{- if and $v (lt (atoi (last (splitList "." $v))) 32) }}{{ fail "envoy.clientIP.trustedCIDRs needs Envoy 1.32 or later (xff_trusted_cidrs); set envoy.tag, or use xffNumTrustedHops" }}{{ end -}}
use_remote_address: false
original_ip_detection_extensions:
  - name: envoy.extensions.http.original_ip_detection.xff
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.http.original_ip_detection.xff.v3.XffConfig
      xff_trusted_cidrs:
        cidrs:
          {{- include "amp.envoyClientIPCIDRs" . | nindent 10 }}
{{- else -}}
use_remote_address: true
xff_num_trusted_hops: {{ $hops }}
{{- end }}
skip_xff_append: false
internal_address_config:
  unix_sockets: false
  cidr_ranges: []
generate_request_id: true
preserve_external_request_id: false
{{- end -}}

{{/* amp.envoyClientIPHeader: route_config entries that put the client
     address Envoy decided (%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%, which
     is the result of the detection above, not the raw TCP peer) in
     X-Amp-Client-Ip, replacing any value a client sent. */}}
{{- define "amp.envoyClientIPHeader" -}}
request_headers_to_add:
  - header: { key: x-amp-client-ip, value: "%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%" }
    append_action: OVERWRITE_IF_EXISTS_OR_ADD
{{- end -}}
