const { withXcodeProject } = require('@expo/config-plugins');

/**
 * `expo prebuild` regenerates ios/ from scratch, which drops the signing team
 * and leaves the next `run:ios` prompting for it interactively. This puts the
 * team back on every prebuild so device builds stay non-interactive.
 */
module.exports = function withDevelopmentTeam(config, { teamId }) {
  return withXcodeProject(config, (cfg) => {
    const project = cfg.modResults;
    const configurations = project.pbxXCBuildConfigurationSection();

    for (const key of Object.keys(configurations)) {
      const buildSettings = configurations[key].buildSettings;
      if (!buildSettings || buildSettings.PRODUCT_NAME === undefined) continue;
      buildSettings.DEVELOPMENT_TEAM = teamId;
      buildSettings.CODE_SIGN_STYLE = 'Automatic';
    }

    project.addTargetAttribute('DevelopmentTeam', teamId);
    return cfg;
  });
};
