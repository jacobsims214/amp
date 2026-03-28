odoo.define('amp_project.dashboard', function (require) {
    "use strict";
    
    var publicWidget = require('web.public.widget');
    var core = require('web.core');
    var _t = core._t;
    
    publicWidget.registry.AmpDashboard = publicWidget.Widget.extend({
        selector: '.o_amp_dashboard',
        
        init: function (parent, options) {
            this._super.apply(this, arguments);
            this.projectId = this.$el.data('project-id');
            this.updateInterval = null;
        },
        
        start: function () {
            var self = this;
            this._super.apply(this, arguments);
            
            // Initial load
            this._updateDashboard();
            
            // Set up auto-refresh every 10 seconds
            this.updateInterval = setInterval(function () {
                self._updateDashboard();
            }, 10000);
            
            return Promise.resolve();
        },
        
        destroy: function () {
            if (this.updateInterval) {
                clearInterval(this.updateInterval);
            }
            this._super.apply(this, arguments);
        },
        
        _updateDashboard: function () {
            var self = this;
            if (!this.projectId) return;
            
            this._rpc({
                route: '/amp/project/' + this.projectId + '/realtime',
                params: {},
            }).then(function (data) {
                self._renderStats(data);
                self._renderActivity(data);
            }).catch(function (error) {
                console.error('Failed to update dashboard:', error);
            });
        },
        
        _renderStats: function (data) {
            // Update stat counters
            var stats = data.stats;
            this.$('.o_stat_epics').text(stats.epic_count);
            this.$('.o_stat_stories').text(stats.story_count);
            this.$('.o_stat_tasks').text(stats.task_count);
            this.$('.o_stat_completed').text(stats.completed_count);
            this.$('.o_stat_blocked').text(stats.blocked_count);
            this.$('.o_stat_progress').text(stats.progress.toFixed(1) + '%');
            
            // Update progress bar
            this.$('.o_progressbar_value').css('width', stats.progress + '%');
        },
        
        _renderActivity: function (data) {
            // Render recent activity
            var $activity = this.$('.o_amp_activity');
            if (!$activity.length) return;
            
            var html = '';
            data.recent_activity.forEach(function (item) {
                html += '<div class="o_activity_item">';
                html += '<span class="o_activity_task">' + item.task_name + '</span>';
                html += '<span class="o_activity_state badge badge-' + self._getStateClass(item.state) + '">' + item.state + '</span>';
                if (item.agent) {
                    html += '<span class="o_activity_agent"><i class="fa fa-robot"></i> ' + item.agent + '</span>';
                }
                html += '</div>';
            });
            
            $activity.html(html || '<p class="text-muted">No recent activity</p>');
        },
        
        _getStateClass: function (state) {
            var classes = {
                'backlog': 'secondary',
                'ready': 'info',
                'in_progress': 'primary',
                'review': 'warning',
                'completed': 'success',
                'blocked': 'danger',
            };
            return classes[state] || 'secondary';
        },
    });
    
    return {
        AmpDashboard: publicWidget.registry.AmpDashboard,
    };
});
