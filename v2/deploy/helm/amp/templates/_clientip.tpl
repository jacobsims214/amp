{{/* amp.trustedProxiesEnv: AMP_TRUSTED_PROXIES for every listener that
     writes audit rows (amp-api REST and MCP, authadmin, mcpoauth; the relay
     on remote-m1 takes it too). The services walk X-Forwarded-For right to
     left past these addresses and take the first one that isn't trusted as
     the client (KB decisions/audit-log-v2.md, section 2; #2427, #2430).
     Empty: nothing is trusted and each row records its TCP peer, Envoy's
     pod. Never list an address a client can send from. */}}
{{- define "amp.trustedProxiesEnv" -}}
- name: AMP_TRUSTED_PROXIES
  value: {{ join "," (.Values.clientIP.trustedProxies | default list) | quote }}
{{- end -}}

{{/* amp.envoyClientIP: the client-IP settings of Envoy's
     HttpConnectionManager, the same on every listener that routes to AMP
     (#2430). Envoy is the first hop the chart owns: it appends its TCP peer
     (Traefik's pod in production, 127.0.0.1 through kubectl port-forward)
     to X-Forwarded-For and trusts no hop before it; the services choose the
     client with AMP_TRUSTED_PROXIES. No address counts as internal, so
     Envoy always replaces a client-sent x-request-id and strips x-envoy-*
     headers, whatever the peer. Side effect: X-Forwarded-Proto becomes
     Envoy's own scheme (http when TLS ends at Traefik); nothing reads it. */}}
{{- define "amp.envoyClientIP" -}}
use_remote_address: true
xff_num_trusted_hops: 0
skip_xff_append: false
internal_address_config:
  unix_sockets: false
  cidr_ranges: []
generate_request_id: true
preserve_external_request_id: false
{{- end -}}
